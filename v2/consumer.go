package liftbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nuid"

	proto "github.com/liftbridge-io/liftbridge-api/go"
)

const defaultAutoCheckpointInterval = 5 * time.Second

func defaultFetchAssignmentsInterval(timeout time.Duration) time.Duration {
	return time.Duration(0.7 * float64(timeout))
}

// AutoOffset determines behavior for where a consumer should start consuming a
// stream partition when the consumer group does not have a committed offset,
// e.g. because the group was just created.
type AutoOffset int

const (
	// autoOffsetNewOnly sets the default start position to the end of the
	// partition, i.e. only new messages will be received.
	autoOffsetNewOnly AutoOffset = iota

	// autoOffsetEarliest sets the default start position to the earliest
	// message received in the partition.
	autoOffsetEarliest

	// autoOffsetLatest sets the default start position to the last message
	// received in the partition.
	autoOffsetLatest

	// autoOffsetNone will cause an error to be sent on the Handler if no
	// previous offset is found for the consumer group.
	autoOffsetNone
)

// ConsumerOptions are used to configure new consumers.
type ConsumerOptions struct {
	// ConsumerID uniquely identifies a logical consumer. This ID should not be
	// reused across consumers. If a ConsumerID is not supplied, a random one
	// will be generated.
	ConsumerID string

	// AutoCheckpointInterval determines the frequency the consumer's positions
	// are committed to Liftbridge. A value of 0 disables auto checkpointing.
	// The default value is 5 seconds if not set.
	AutoCheckpointInterval time.Duration

	// AutoOffsetDefault determines the behavior for where a consumer should
	// start consuming a stream partition when the consumer group does not have
	// a committed offset, e.g. because the group was just created. If not set,
	// defaults to AutoOffsetNewOnly.
	AutoOffset AutoOffset

	// FetchAssignmentsInterval is a function which returns the frequency to
	// fetch partition assignments from the consumer group coordinator. This
	// also acts as a health check to keep the consumer active in the group.
	// Increasing this too much may cause the group coordinator to think the
	// consumer has failed and remove it from the group. The function argument
	// is the timeout duration configured on the server. If not set, this will
	// default to 0.7 * timeout.
	FetchAssignmentsInterval func(timeout time.Duration) time.Duration
}

// ConsumerOption is a function on the ConsumerOptions for a consumer. These
// are used to configure particular consumer options.
type ConsumerOption func(*ConsumerOptions) error

// AutoCheckpoint determines the frequency in which the consumer's positions
// should be committed to Liftbridge. A value of 0 disables auto checkpointing.
// Defaults to 5 seconds if not set.
func AutoCheckpoint(interval time.Duration) ConsumerOption {
	return func(o *ConsumerOptions) error {
		o.AutoCheckpointInterval = interval
		return nil
	}
}

// AutoOffsetEarliest sets the default start position to the earliest message
// received in the partition.
func AutoOffsetEarliest() ConsumerOption {
	return func(o *ConsumerOptions) error {
		o.AutoOffset = autoOffsetEarliest
		return nil
	}
}

// AutoOffsetLatest sets the default start position to the last message
// received in the partition.
func AutoOffsetLatest() ConsumerOption {
	return func(o *ConsumerOptions) error {
		o.AutoOffset = autoOffsetLatest
		return nil
	}
}

// AutoOffsetNone will cause an error to be sent on the Handler if no previous
// offset is found for the consumer group.
func AutoOffsetNone() ConsumerOption {
	return func(o *ConsumerOptions) error {
		o.AutoOffset = autoOffsetNone
		return nil
	}
}

// ConsumerID uniquely identifies a logical consumer. This ID should not be
// reused across consumers. If a ConsumerID is not supplied, a random one
// will be generated.
func ConsumerID(id string) ConsumerOption {
	return func(o *ConsumerOptions) error {
		if id == "" {
			return errors.New("invalid consumer id")
		}
		o.ConsumerID = id
		return nil
	}
}

type subscription struct {
	offset              int64
	lastCommittedOffset int64
	ctxCancel           context.CancelFunc
}

func (s *subscription) cancel() {
	if s.ctxCancel != nil {
		s.ctxCancel()
	}
}

type streamSubscriptions map[int32]*subscription

// Consumer is a member of a consumer group. Consumer groups provide an API to
// better facilitate the consumption of Liftbridge streams. This encompasses
// several different but related goals:
// 1) Provide a mechanism for clients to track their position in a stream
// automatically, i.e. "durable" consumers.
// 2) Provide a mechanism for distributed, fault-tolerant stream consumption.
// 3) Provide a mechanism for coordinating and balancing stream consumption by
// managing partition assignments for consumers.
// 4) Provide a mechanism for consuming multiple streams in aggregate.
type Consumer struct {
	opts          *ConsumerOptions
	groupID       string
	client        *client
	closed        chan struct{}
	mu            sync.RWMutex
	subscriptions map[string]streamSubscriptions
	ctx           context.Context
	cancelCtx     context.CancelFunc
}

func (c *client) newConsumer(groupID string, options []ConsumerOption) (*Consumer, error) {
	if groupID == "" {
		return nil, errors.New("invalid consumer group id")
	}
	opts := &ConsumerOptions{
		AutoCheckpointInterval:   defaultAutoCheckpointInterval,
		FetchAssignmentsInterval: defaultFetchAssignmentsInterval,
	}
	for _, opt := range options {
		if err := opt(opts); err != nil {
			return nil, err
		}
	}
	if opts.ConsumerID == "" {
		opts.ConsumerID = nuid.Next()
	}

	cons := &Consumer{
		opts:          opts,
		groupID:       groupID,
		client:        c,
		closed:        make(chan struct{}),
		subscriptions: make(map[string]streamSubscriptions),
	}
	return cons, nil
}

// Subscribe begins consuming from assigned partitions. If no partitions are
// assigned to this consumer, this will wait for partitions to be assigned.
func (c *Consumer) Subscribe(ctx context.Context, streams []string, handler Handler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx != nil {
		return errors.New("subscribe has already been called - cancel previous Context before calling Subscribe again")
	}

	joinReq := &proto.JoinConsumerGroupRequest{
		GroupId:    c.groupID,
		ConsumerId: c.opts.ConsumerID,
		Streams:    streams,
	}
	var resp *proto.JoinConsumerGroupResponse
	err := c.client.doResilientRPC(ctx, func(client proto.APIClient) error {
		r, err := client.JoinConsumerGroup(ctx, joinReq)
		if err != nil {
			return err
		}
		resp = r
		return nil
	})
	if err != nil {
		return err
	}

	interval := c.opts.FetchAssignmentsInterval(time.Duration(resp.ConsumersTimeout))

	var client proto.APIClient
	for i := 0; i < 5; i++ {
		client, err = c.client.getAPIClientForBroker(resp.Coordinator)
		if err != nil {
			sleepContext(ctx, 50*time.Millisecond)
			c.client.updateMetadata(ctx)
			continue
		}
		c.ctx, c.cancelCtx = context.WithCancel(ctx)
		go c.consumerLoop(c.ctx, client, interval, resp.CoordinatorEpoch, c.wrapHandler(handler))
		if c.opts.AutoCheckpointInterval > 0 {
			go c.checkpointLoop(c.ctx)
		}
		return nil
	}
	return err
}

type cursor struct {
	sub    *subscription
	offset int64
}

// Checkpoint commits the consumer's current offset positions for the stream
// partitions it is subscribed to. The offsets that are committed will be used
// after each group rebalance or on consumer startup to allow consumers to pick
// up where they left off. This is intended to be used if auto checkpointing is
// disabled and the consumer needs more fine-grained control over when offsets
// are committed, e.g. to avoid redelivery of processed messages.
func (c *Consumer) Checkpoint(ctx context.Context) error {
	c.mu.RLock()
	if c.ctx == nil {
		c.mu.RUnlock()
		return errors.New("consumer is not currently subscribed to any streams")
	}
	cursors := make(map[string]map[int32]cursor)
	for stream, streamSubscriptions := range c.subscriptions {
		for partition, subscription := range streamSubscriptions {
			offset := atomic.LoadInt64(&subscription.offset)
			if offset == -1 {
				continue
			}
			lastCommittedOffset := atomic.LoadInt64(&subscription.lastCommittedOffset)
			if lastCommittedOffset == offset {
				continue
			}
			streamCursors, ok := cursors[stream]
			if !ok {
				streamCursors = make(map[int32]cursor)
				cursors[stream] = streamCursors
			}
			streamCursors[partition] = cursor{sub: subscription, offset: offset}
		}
	}
	c.mu.RUnlock()

	for stream, partitions := range cursors {
		for partition, cursor := range partitions {
			if err := c.client.SetCursor(ctx, c.groupID, stream, partition, cursor.offset); err != nil {
				return err
			}
			atomic.StoreInt64(&cursor.sub.lastCommittedOffset, cursor.offset)
		}
	}
	return nil
}

// Close the consumer and remove them from the consumer group.
func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.closed:
		return nil
	default:
	}

	if c.ctx != nil {
		c.cancelCtx()
		c.ctx = nil
	}

	// TODO make RPC to leave group

	close(c.closed)
	c.subscriptions = make(map[string]streamSubscriptions)
	return nil
}

func (c *Consumer) checkpointLoop(ctx context.Context) {
	for {
		select {
		case <-c.closed:
			return
		case <-time.After(c.opts.AutoCheckpointInterval):
			// I'm not sure if there's much we can do with the error?
			_ = c.Checkpoint(ctx)
		}
	}
}

func (c *Consumer) consumerLoop(ctx context.Context, client proto.APIClient,
	interval time.Duration, coordinatorEpoch uint64, handler Handler) {

	for {
		req := &proto.FetchConsumerGroupAssignmentsRequest{
			GroupId:          c.groupID,
			ConsumerId:       c.opts.ConsumerID,
			CoordinatorEpoch: coordinatorEpoch,
		}
		var resp *proto.FetchConsumerGroupAssignmentsResponse
		err := c.client.doResilientRPC(ctx, func(client proto.APIClient) error {
			r, err := client.FetchConsumerGroupAssignments(ctx, req)
			if err != nil {
				return err
			}
			resp = r
			return nil
		})
		if err != nil {
			panic(err)
			// TODO: need to handle this somehow
			// extend FetchMetadata to include info on consumer groups
		}

		c.reconcileSubscriptions(ctx, resp.Assignments, resp.AssignmentEpoch, handler)

		select {
		case <-c.closed:
			return
		case <-time.After(interval):
		}
	}
}

func (c *Consumer) reconcileSubscriptions(ctx context.Context, assignments []*proto.PartitionAssignment,
	assignmentEpoch uint64, handler Handler) {

	assignmentsMap := make(map[string]map[int32]struct{}, len(assignments))
	for _, assignment := range assignments {
		m := make(map[int32]struct{}, len(assignment.Partitions))
		for _, partition := range assignment.Partitions {
			m[partition] = struct{}{}
		}
		assignmentsMap[assignment.Stream] = m
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Cancel all subscriptions that we no longer have assignments for.
	for stream, subscriptions := range c.subscriptions {
		for partition, subscription := range subscriptions {
			streamAssignments, ok := assignmentsMap[stream]
			if !ok {
				subscription.cancel()
				delete(subscriptions, partition)
			}
			if _, ok := streamAssignments[partition]; !ok {
				subscription.cancel()
				delete(subscriptions, partition)
			}
			if len(subscriptions) == 0 {
				delete(c.subscriptions, stream)
			}
		}
	}

	// Ensure we have subscriptions for partitions we do have assignments for.
	for stream, assignments := range assignmentsMap {
		subscriptions, ok := c.subscriptions[stream]
		if !ok {
			subscriptions = make(streamSubscriptions)
			c.subscriptions[stream] = subscriptions
		}
		for partition := range assignments {
			// Check if subscription already exists for partition.
			if _, ok := subscriptions[partition]; ok {
				continue
			}
			// Otherwise set up a new subscription.
			cancel, err := c.subscribeToPartition(ctx, stream, partition, assignmentEpoch, handler)
			if err != nil {
				// TODO: should we wrap this error?
				go handler(nil, err)
				continue
			}
			subscriptions[partition] = &subscription{
				offset:    -1,
				ctxCancel: cancel,
			}
		}
	}
}

func (c *Consumer) subscribeToPartition(ctx context.Context, stream string, partition int32,
	assignmentEpoch uint64, handler Handler) (context.CancelFunc, error) {

	startPosition, err := c.getStartPosition(ctx, stream, partition)
	if err != nil {
		return nil, err
	}

	subCtx, cancel := context.WithCancel(ctx)
	if err := c.client.Subscribe(subCtx, stream, handler, Partition(partition),
		startPosition, consumer(c.groupID, c.opts.ConsumerID, assignmentEpoch)); err != nil {
		cancel()
		return nil, err
	}

	return cancel, nil
}

func (c *Consumer) getStartPosition(ctx context.Context, stream string, partition int32) (SubscriptionOption, error) {
	cursor, err := c.client.FetchCursor(ctx, c.groupID, stream, partition)
	if err != nil {
		return nil, err
	}
	var startPosition SubscriptionOption
	if cursor == -1 {
		// Cursor doesn't exist. Use auto offset behavior.
		switch c.opts.AutoOffset {
		case autoOffsetNewOnly:
			startPosition = StartAtNewOnly()
		case autoOffsetEarliest:
			startPosition = StartAtEarliestReceived()
		case autoOffsetLatest:
			startPosition = StartAtLatestReceived()
		case autoOffsetNone:
			fallthrough
		default:
			return nil, fmt.Errorf("no previous consumer group offset found for partition %d of stream %s", partition, stream)
		}
	} else {
		startPosition = StartAtOffset(cursor + 1)
	}
	return startPosition, nil
}

func (c *Consumer) wrapHandler(handler Handler) Handler {
	return func(msg *Message, err error) {
		if msg != nil {
			subscription := c.subscriptions[msg.Stream()][msg.Partition()]
			atomic.StoreInt64(&subscription.offset, msg.Offset())
		}
		handler(msg, err)
	}
}
