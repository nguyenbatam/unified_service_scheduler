package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/segmentio/kafka-go"
)

type Bus struct {
	writer *kafka.Writer
	// write is writer.WriteMessages (replaced in tests); lifetime bounds one call (see
	// MaxWriteLifetime).
	write    func(ctx context.Context, msgs ...kafka.Message) error
	lifetime time.Duration
	// maxCount/maxBytes split a call so it never queues more than one kafka-go batch per
	// partition (0 = no limit; tests).
	maxCount int
	maxBytes int64
	// now reads the clock the caller's deadline is compared against (nil = time.Now; tests).
	now   func() time.Time
	topic string
	log   *slog.Logger
}

// Writer limits. kafka-go keeps retrying a queued batch in the background even after the
// WriteMessages call that queued it has returned (its context ended), so these numbers, not the
// caller's context, bound how long a published message can still land. See MaxWriteLifetime.
const (
	writeTimeout    = 2 * time.Second // one produce request
	writeAttempts   = 3               // produce requests per batch (kafka-go retries temporary errors)
	writeBackoffMax = 500 * time.Millisecond
	// kafka-go waits up to BatchTimeout (default 1s) before flushing a partial batch; with
	// synchronous writes that was ~1s of latency per outbox row.
	batchTimeout = 10 * time.Millisecond
)

// WriterBatchSize and WriterBatchBytes are the writer's per-partition batch limits, set explicitly
// rather than left to kafka-go's defaults (100 messages, 1 MiB). A call larger than either would
// queue a second batch for a partition behind the first, finishing up to twice MaxWriteLifetime
// later; PublishBatch splits its calls to stay within both, and worker.OutboxConfig.Validate keeps
// a claim within WriterBatchSize.
const (
	WriterBatchSize  = 100
	WriterBatchBytes = 1 << 20
)

// MaxWriteLifetime is the longest kafka-go can keep one batch alive: the flush wait plus every
// attempt and the backoff between them. kafka-go's backoff before retry n is 100ms·n², capped at
// WriteBackoffMax (100ms + 400ms = 500ms for our 3 attempts); the bound charges WriteBackoffMax per
// retry (1s), which stays conservative if the attempt count grows. The outbox lease must cover a
// publish deadline plus this, so a batch abandoned by a timed-out call is dead before any instance
// may publish its rows again (see worker.OutboxConfig.Validate). It holds for one batch per
// partition per call, which PublishBatch guarantees (WriterBatchSize, WriterBatchBytes).
const MaxWriteLifetime = batchTimeout + writeAttempts*writeTimeout + (writeAttempts-1)*writeBackoffMax

func NewBus(brokers []string, topic string, log *slog.Logger) *Bus {
	writer := &kafka.Writer{
		Addr:  kafka.TCP(brokers...),
		Topic: topic,
		// Hash on the key (aggregate id) so one appointment's events share a partition and
		// keep their order. LeastBytes ignores keys.
		Balancer:        &kafka.Hash{},
		RequiredAcks:    kafka.RequireAll,
		BatchTimeout:    batchTimeout,
		WriteTimeout:    writeTimeout,
		MaxAttempts:     writeAttempts,
		WriteBackoffMax: writeBackoffMax,
		BatchSize:       WriterBatchSize,
		BatchBytes:      WriterBatchBytes,
		Async:           false,
	}
	return &Bus{
		writer: writer, write: writer.WriteMessages, lifetime: MaxWriteLifetime,
		maxCount: WriterBatchSize, maxBytes: WriterBatchBytes, topic: topic, log: log,
	}
}

// EnsureTopic creates topic with the given layout if it does not exist yet; an existing topic is
// left as it is. Call it before starting consumers: brokers may refuse auto-creation, and a group
// that joins before the topic exists starts with no partitions. Keys (aggregate ids) pin each
// appointment to one partition, so more partitions keep per-appointment order and let the shared
// notify group spread its work across instances.
func EnsureTopic(ctx context.Context, brokers []string, topic string, partitions, replication int) error {
	var errs []error
	for _, broker := range brokers {
		err := createTopic(ctx, broker, kafka.TopicConfig{Topic: topic, NumPartitions: partitions, ReplicationFactor: replication})
		if err == nil || errors.Is(err, kafka.TopicAlreadyExists) {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", broker, err))
	}
	return fmt.Errorf("ensure topic %s: %w", topic, errors.Join(errs...))
}

// KeepEnsuringTopic retries EnsureTopic with backoff until it succeeds or ctx ends. The outbox
// cannot publish to a missing topic (kafka-go never asks the broker to auto-create it), so a
// startup failure (Kafka slow to elect a controller, a transient network error) must not be final;
// events wait in the outbox meanwhile.
func KeepEnsuringTopic(ctx context.Context, brokers []string, topic string, partitions, replication int, log *slog.Logger) {
	backoff := time.Second
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := EnsureTopic(attempt, brokers, topic, partitions, replication)
		cancel()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("kafka topic not ready; retrying", "err", err, "topic", topic, "in", backoff)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// createTopic asks the cluster controller (found through broker) to create the topic.
func createTopic(ctx context.Context, broker string, topic kafka.TopicConfig) error {
	conn, err := kafka.DialContext(ctx, "tcp", broker)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	cc, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	return cc.CreateTopics(topic)
}

func (b *Bus) Close() error {
	return b.writer.Close()
}

// Handler processes one event. A non-nil error is retried with backoff before the offset is
// committed (at-least-once), so handlers must be idempotent (e.g. dedupe on Event.ID). A panic is
// recovered and retried like an error, but only maxHandlerPanics times (see Consumer.process).
type Handler func(ctx context.Context, ev realtime.Event) error

// PanicError is a handler panic turned into an error (with the goroutine's stack at the panic).
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("event handler panicked: %v", e.Value) }

// call runs h and turns a panic into a *PanicError, so one bad event (or a bug it triggers) never
// takes the process down: every replica would read the same message on restart and crash again.
func call(ctx context.Context, h Handler, ev realtime.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return h(ctx, ev)
}

// Broadcaster adapts a WebSocket hub: local fan-out never fails.
func Broadcaster(hub *realtime.Hub) Handler {
	return func(_ context.Context, ev realtime.Event) error {
		hub.Broadcast(ev)
		return nil
	}
}

// Chain runs every handler in order for each event and joins their errors. A failure retries the
// whole chain, so each member must tolerate seeing the event again. A member's panic is recovered
// (as a *PanicError) so the members after it still run.
func Chain(handlers ...Handler) Handler {
	return func(ctx context.Context, ev realtime.Event) error {
		var errs []error
		for _, h := range handlers {
			if err := call(ctx, h, ev); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// messageReader is the slice of *kafka.Reader a Consumer uses (fakeable in tests).
type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

type Consumer struct {
	reader  messageReader
	handle  Handler
	name    string
	brokers []string
	// deleteGroup: Close removes the (per-boot) group from the broker once the reader has left.
	deleteGroup bool
	// skipPayload: decode events without Payload (SkipPayload).
	skipPayload bool
	log         *slog.Logger
}

// StartOffset is where a consumer group with no committed offset (new, or expired) begins.
type StartOffset int64

const (
	// FromLatest skips history: for per-instance fan-out groups, which only serve the sockets open
	// now (a restarted instance must not replay days of events to its clients).
	FromLatest StartOffset = StartOffset(kafka.LastOffset)
	// FromEarliest replays everything the topic retains: for shared work groups (notifications)
	// whose handlers are idempotent (dedupe_key), so events produced before the group first
	// committed — seed bookings on a fresh stack, or after its offsets expired — are not lost.
	FromEarliest StartOffset = StartOffset(kafka.FirstOffset)
)

type consumerConfig struct {
	reader      kafka.ReaderConfig
	deleteGroup bool
	skipPayload bool
}

// ConsumerOption adjusts a consumer.
type ConsumerOption func(*consumerConfig)

// StartFrom sets where the group starts when it has no committed offset (default FromLatest).
func StartFrom(start StartOffset) ConsumerOption {
	return func(c *consumerConfig) { c.reader.StartOffset = int64(start) }
}

// DeleteGroupOnClose makes Close delete the consumer group from the broker (best effort, after the
// reader has left it): for per-boot fan-out groups (see FanoutGroupID), which no later run reuses.
func DeleteGroupOnClose() ConsumerOption {
	return func(c *consumerConfig) { c.deleteGroup = true }
}

// SkipPayload decodes events without their Payload (the full aggregate, with customer PII): for
// consumers whose handlers only route on the envelope, such as the WebSocket fan-out. Handlers
// then always see a nil Event.Payload. The notifier needs it and must not use this.
func SkipPayload() ConsumerOption {
	return func(c *consumerConfig) { c.skipPayload = true }
}

// deleteGroupTimeout bounds the best-effort group deletion on shutdown.
const deleteGroupTimeout = 3 * time.Second

// FanoutGroupID names a per-boot WebSocket fan-out group: the instance id plus a random nonce.
// A stable id would resume from the offsets the previous run committed and replay what happened
// while the instance was down — including SESSION_REVOKED events, which would close sockets opened
// since. With FromLatest every boot starts live. The group still commits: kafka-go re-reads the
// committed offsets at every new generation (coordinator move, session timeout, partition change)
// and falls back to StartOffset only when there are none, so an uncommitted group would jump to
// the end of the topic on each rebalance and silently drop what came in between. Close deletes the
// group (DeleteGroupOnClose); one left behind by a crash expires after offsets.retention.minutes.
func FanoutGroupID(instanceID string) string {
	return "scheduler-ws-" + instanceID + "-" + uuid.NewString()[:8]
}

// NewConsumer reads topic as groupID and commits what it has handled. Every instance gets its own
// per-boot group for WebSocket fan-out (each sees every event; FromLatest, DeleteGroupOnClose);
// work that must happen once (notifications) shares one long-lived group, so the partitions are
// split between instances, and starts FromEarliest.
func NewConsumer(brokers []string, topic, groupID string, handle Handler, log *slog.Logger, opts ...ConsumerOption) *Consumer {
	cfg := newConsumerConfig(brokers, topic, groupID, opts...)
	return &Consumer{
		reader:      kafka.NewReader(cfg.reader),
		handle:      handle,
		name:        groupID,
		brokers:     brokers,
		deleteGroup: cfg.deleteGroup,
		skipPayload: cfg.skipPayload,
		log:         log,
	}
}

func newConsumerConfig(brokers []string, topic, groupID string, opts ...ConsumerOption) consumerConfig {
	cfg := consumerConfig{
		reader: kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        groupID,
			MinBytes:       1,
			MaxBytes:       10e6,
			MaxWait:        250 * time.Millisecond,
			CommitInterval: time.Second,
			StartOffset:    int64(FromLatest),
			// Rebalance when the topic gains partitions (including 0 -> n: a group that joined
			// before the topic existed would otherwise keep an empty assignment and miss every
			// event).
			WatchPartitionChanges: true,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Close stops the reader (which commits pending offsets and leaves the group) and, with
// DeleteGroupOnClose, then deletes the group within deleteGroupTimeout. A failed deletion is only
// logged: the broker expires the empty group on its own.
func (c *Consumer) Close() error {
	err := c.reader.Close()
	if c.deleteGroup {
		ctx, cancel := context.WithTimeout(context.Background(), deleteGroupTimeout)
		defer cancel()
		if derr := deleteGroup(ctx, c.brokers, c.name); derr != nil {
			c.log.Warn("kafka delete group", "err", derr, "group", c.name)
		}
	}
	return err
}

// deleteGroup asks the group's coordinator to delete groupID (it must have no members left).
func deleteGroup(ctx context.Context, brokers []string, groupID string) error {
	client := &kafka.Client{Addr: kafka.TCP(brokers...), Timeout: deleteGroupTimeout}
	resp, err := client.DeleteGroups(ctx, &kafka.DeleteGroupsRequest{GroupIDs: []string{groupID}})
	if err != nil {
		return err
	}
	if err := resp.Errors[groupID]; err != nil && !errors.Is(err, kafka.GroupIdNotFound) {
		return err
	}
	return nil
}

// Run hands every event to the handler until ctx ends.
func (c *Consumer) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Error("kafka fetch", "err", err, "group", c.name)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		backoff = 250 * time.Millisecond
		if event, err := c.decode(msg.Value); err != nil {
			c.log.Error("kafka decode", "err", err, "group", c.name) // poison: skip it
		} else if !c.process(ctx, event) {
			return
		}
		if err := c.commit(ctx, msg); err != nil {
			c.log.Error("kafka commit", "err", err, "group", c.name)
		}
	}
}

// skippedPayload is a JSON value the decoder steps over without copying or decoding it.
type skippedPayload struct{}

func (*skippedPayload) UnmarshalJSON([]byte) error { return nil }

// envelope decodes an Event without its payload: the outer Payload field shadows the embedded
// one, so encoding/json hands the "payload" value to skippedPayload.
type envelope struct {
	realtime.Event
	Payload skippedPayload `json:"payload"`
}

// decode parses one message, leaving Payload out when the consumer skips it.
func (c *Consumer) decode(value []byte) (realtime.Event, error) {
	if c.skipPayload {
		var env envelope
		err := json.Unmarshal(value, &env)
		return env.Event, err
	}
	var event realtime.Event
	err := json.Unmarshal(value, &event)
	return event, err
}

// commitTimeout bounds handing a handled message's offset to the reader.
const commitTimeout = 2 * time.Second

// commit records msg as handled. It ignores ctx cancellation: a message handled just before
// shutdown must still be committed (Close flushes what the reader holds), or the shared notify
// group hands it again to whichever instance owns the partition next. A short timeout bounds it
// instead.
func (c *Consumer) commit(ctx context.Context, msg kafka.Message) error {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	return c.reader.CommitMessages(commitCtx, msg)
}

// onlyPanics reports whether err is made of handler panics only (a *PanicError, or a join of
// them, as Chain returns) and returns the first. A chain where one member panicked and another
// returned an ordinary error is retried like an error, so that member's work is never dropped.
func onlyPanics(err error) (*PanicError, bool) {
	if pe, ok := err.(*PanicError); ok { //nolint:errorlint // exact shape: wrapped errors are not panics of ours
		return pe, true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil, false
	}
	var first *PanicError
	for _, e := range joined.Unwrap() {
		pe, ok := onlyPanics(e)
		if !ok {
			return nil, false
		}
		if first == nil {
			first = pe
		}
	}
	return first, first != nil
}

// maxHandlerPanics is how many times an event whose handler panics is tried before it is skipped:
// with the doubling backoff from 250 ms that is ~3.75 s, so a panic caused by a passing state
// (e.g. a database failover) gets a chance to clear before the event is given up.
const maxHandlerPanics = 5

// skippedEvents counts events skipped after repeated handler panics, across consumers.
var skippedEvents atomic.Uint64

// SkippedEvents is the number of events this process skipped after repeated handler panics
// (exported as scheduler_events_skipped_total; any increase is a lost event and alerts).
func SkippedEvents() uint64 { return skippedEvents.Load() }

// process retries the handler until it succeeds; false means ctx ended first (do not commit).
// Errors are retried for as long as it takes (a database or broker outage ends). A panic is a bug,
// usually tied to the event's content, so retrying it forever would stall the partition (every
// replica, for the fan-out): after maxHandlerPanics panics the event is logged and skipped
// (committed), like an undecodable message.
func (c *Consumer) process(ctx context.Context, ev realtime.Event) bool {
	backoff := 250 * time.Millisecond
	panics := 0
	for {
		err := call(ctx, c.handle, ev)
		if err == nil {
			return true
		}
		if pe, ok := onlyPanics(err); ok {
			panics++
			c.log.Error("event handler panicked", "panic", pe.Value, "group", c.name, "type", ev.Type, "id", ev.ID,
				"attempt", panics, "stack", string(pe.Stack))
			if panics >= maxHandlerPanics {
				c.log.Error("skipping event after repeated handler panics", "group", c.name, "type", ev.Type, "id", ev.ID)
				skippedEvents.Add(1)
				return true
			}
		} else {
			c.log.Warn("event handler failed; retrying", "err", err, "group", c.name, "type", ev.Type)
		}
		if !sleep(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
