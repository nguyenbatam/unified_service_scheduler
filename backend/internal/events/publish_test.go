package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/segmentio/kafka-go"
)

// fakeWriter scripts kafka-go outcomes per call: respond gets the keys of one WriteMessages call.
type fakeWriter struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(ctx context.Context, keys []string, msgs []kafka.Message) error
}

func (f *fakeWriter) write(ctx context.Context, msgs ...kafka.Message) error {
	keys := make([]string, len(msgs))
	for i, m := range msgs {
		keys[i] = string(m.Key)
	}
	f.mu.Lock()
	f.calls = append(f.calls, keys)
	f.mu.Unlock()
	return f.respond(ctx, keys, msgs)
}

// perKey answers with kafka.WriteErrors built from a per-key error (nil = written).
func perKey(errFor map[string]error) func(context.Context, []string, []kafka.Message) error {
	return func(_ context.Context, keys []string, _ []kafka.Message) error {
		werr := make(kafka.WriteErrors, len(keys))
		failed := false
		for i, k := range keys {
			werr[i] = errFor[k]
			failed = failed || werr[i] != nil
		}
		if !failed {
			return nil
		}
		return werr
	}
}

func testBus(f *fakeWriter, lifetime time.Duration) *Bus {
	return &Bus{write: f.write, lifetime: lifetime}
}

func envelopes(keys ...string) []Envelope {
	out := make([]Envelope, len(keys))
	for i, k := range keys {
		out[i] = Envelope{Key: k, Event: realtime.Event{Type: domain.EventType(99), Kind: realtime.KindConfig}}
	}
	return out
}

type outcome int

const (
	ok outcome = iota
	retriable
	rejected
)

func classify(err error) outcome {
	switch {
	case err == nil:
		return ok
	case IsRetriable(err):
		return retriable
	default:
		return rejected
	}
}

func TestPublishBatchClassifiesKafkaErrors(t *testing.T) {
	tests := []struct {
		name      string
		errFor    map[string]error // first call; later (solo) calls use solo
		solo      map[string]error
		want      []outcome // per a, b, c; nil = whole-batch failure expected
		wantCalls int
	}{
		{
			name: "all written", want: []outcome{ok, ok, ok}, wantCalls: 1,
		},
		{
			// kafka-go fills WriteErrors for every message of a failed partition batch: a
			// cluster-wide temporary error is a whole-batch failure, never a rejection.
			name:      "same temporary error everywhere",
			errFor:    map[string]error{"a": kafka.NotEnoughReplicas, "b": kafka.NotEnoughReplicas, "c": kafka.NotEnoughReplicas},
			wantCalls: 1,
		},
		{
			name:      "mixed temporary errors everywhere",
			errFor:    map[string]error{"a": kafka.NotLeaderForPartition, "b": kafka.RequestTimedOut, "c": context.DeadlineExceeded},
			wantCalls: 1,
		},
		{
			name:      "one partition temporarily down",
			errFor:    map[string]error{"b": kafka.NotLeaderForPartition},
			want:      []outcome{ok, retriable, ok},
			wantCalls: 1,
		},
		{
			// Not about any one message (would hit them all): never counted.
			name:      "non-temporary cluster error",
			errFor:    map[string]error{"a": kafka.TopicAuthorizationFailed, "b": kafka.TopicAuthorizationFailed, "c": kafka.TopicAuthorizationFailed},
			wantCalls: 1,
		},
		{
			name:      "lone content rejection is unambiguous",
			errFor:    map[string]error{"b": kafka.MessageSizeTooLarge},
			want:      []outcome{ok, rejected, ok},
			wantCalls: 1,
		},
		{
			name:      "content rejection next to a temporary error",
			errFor:    map[string]error{"a": kafka.NotEnoughReplicas, "b": kafka.InvalidRecord},
			want:      []outcome{retriable, rejected, ok},
			wantCalls: 1,
		},
		{
			// One oversized record fails its whole partition batch: resend each alone to find it.
			name:      "shared content rejection is isolated",
			errFor:    map[string]error{"a": kafka.MessageSizeTooLarge, "b": kafka.MessageSizeTooLarge, "c": kafka.MessageSizeTooLarge},
			solo:      map[string]error{"b": kafka.MessageSizeTooLarge},
			want:      []outcome{ok, rejected, ok},
			wantCalls: 4,
		},
		{
			name:      "isolation hits a transient error",
			errFor:    map[string]error{"a": kafka.InvalidMessage, "b": kafka.InvalidMessage},
			solo:      map[string]error{"a": kafka.LeaderNotAvailable, "b": kafka.InvalidMessage},
			want:      []outcome{retriable, rejected, ok},
			wantCalls: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeWriter{}
			first := true
			f.respond = func(ctx context.Context, keys []string, msgs []kafka.Message) error {
				if first {
					first = false
					return perKey(tt.errFor)(ctx, keys, msgs)
				}
				return perKey(tt.solo)(ctx, keys, msgs)
			}
			errs, err := testBus(f, time.Second).PublishBatch(t.Context(), envelopes("a", "b", "c"))
			if len(f.calls) != tt.wantCalls {
				t.Fatalf("write calls = %v, want %d", f.calls, tt.wantCalls)
			}
			if tt.want == nil {
				if err == nil {
					t.Fatalf("want whole-batch failure, got per-message %v", errs)
				}
				if isRejection(err) {
					t.Fatalf("whole-batch error %v must not look like a rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected whole-batch error %v", err)
			}
			for i, w := range tt.want {
				if got := classify(errs[i]); got != w {
					t.Fatalf("message %d: outcome %d (%v), want %d", i, got, errs[i], w)
				}
			}
		})
	}
}

// kafka-go refuses the whole call when one message exceeds BatchBytes: that one is rejected and
// the others are sent again right away.
func TestPublishBatchClientSideTooLarge(t *testing.T) {
	f := &fakeWriter{}
	f.respond = func(_ context.Context, keys []string, msgs []kafka.Message) error {
		for i, k := range keys {
			if k == "b" {
				return kafka.MessageTooLargeError{Message: msgs[i]}
			}
		}
		return nil
	}
	errs, err := testBus(f, time.Second).PublishBatch(t.Context(), envelopes("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if classify(errs[0]) != ok || classify(errs[1]) != rejected || classify(errs[2]) != ok {
		t.Fatalf("errs = %v", errs)
	}
	if len(f.calls) != 2 || len(f.calls[1]) != 2 {
		t.Fatalf("calls = %v, want the two others resent together", f.calls)
	}
}

// A call abandoned at its deadline may still land: the caller must hear that the outcome is
// unknown (to keep the rows back until kafka-go has given up), not a plain failure.
func TestPublishBatchAbandonedCallIsOutcomeUnknown(t *testing.T) {
	f := &fakeWriter{respond: func(ctx context.Context, _ []string, _ []kafka.Message) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	_, err := testBus(f, 50*time.Millisecond).PublishBatch(t.Context(), envelopes("a"))
	if err == nil || !IsOutcomeUnknown(err) {
		t.Fatalf("err = %v, want outcome unknown", err)
	}
}

// No write starts unless the caller's deadline leaves the writer's whole lifetime: kafka-go keeps
// retrying a batch after the call returns, so a later start could outlive the outbox lease.
func TestPublishBatchNeedsLifetimeBeforeDeadline(t *testing.T) {
	f := &fakeWriter{respond: perKey(nil)}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := testBus(f, time.Second).PublishBatch(ctx, envelopes("a"))
	if !errors.Is(err, errNoTime) || IsOutcomeUnknown(err) {
		t.Fatalf("err = %v, want errNoTime (nothing sent)", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.calls)
	}
}

// Isolation stops starting solo writes once the deadline is too close; the remaining suspects are
// retried later without counting a rejection. The bus reads a fake clock (only the shared write
// advances it), so the outcome does not depend on how fast the test runs.
func TestPublishBatchIsolationRespectsDeadline(t *testing.T) {
	// The real deadline is an hour away (nothing expires while the test runs); the fake clock
	// starts 300ms before it, against a 200ms writer lifetime: the shared write may start, a solo
	// resend after it may not.
	deadline := time.Now().Add(time.Hour)
	var mu sync.Mutex
	now := deadline.Add(-300 * time.Millisecond)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }

	f := &fakeWriter{}
	f.respond = func(ctx context.Context, keys []string, msgs []kafka.Message) error {
		if len(keys) > 1 {
			advance(150 * time.Millisecond) // the shared failure eats the budget
			return perKey(map[string]error{"a": kafka.MessageSizeTooLarge, "b": kafka.MessageSizeTooLarge})(ctx, keys, msgs)
		}
		return nil
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	bus := testBus(f, 200*time.Millisecond)
	bus.now = clock
	errs, err := bus.PublishBatch(ctx, envelopes("a", "b", "c"))
	if err != nil {
		t.Fatalf("whole-batch error %v", err)
	}
	if classify(errs[0]) != retriable || classify(errs[1]) != retriable || classify(errs[2]) != ok {
		t.Fatalf("errs = %v, want a and b retriable (not isolated in time), c written", errs)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v, want only the shared write", f.calls)
	}
}

func TestMaxWriteLifetimeCoversWriterRetries(t *testing.T) {
	b := NewBus([]string{"127.0.0.1:1"}, "t", nil)
	defer func() { _ = b.Close() }()
	w := b.writer
	worst := w.BatchTimeout + time.Duration(w.MaxAttempts)*w.WriteTimeout + time.Duration(w.MaxAttempts-1)*w.WriteBackoffMax
	if MaxWriteLifetime < worst || b.lifetime != MaxWriteLifetime {
		t.Fatalf("MaxWriteLifetime = %s, writer worst case %s", MaxWriteLifetime, worst)
	}
}

// The writer's batch limits are explicit and the bus splits calls to them, so one call never
// queues a second batch per partition (which could finish up to 2x MaxWriteLifetime later).
func TestBusUsesExplicitWriterBatchLimits(t *testing.T) {
	b := NewBus([]string{"127.0.0.1:1"}, "t", nil)
	defer func() { _ = b.Close() }()
	if b.writer.BatchSize != WriterBatchSize || b.writer.BatchBytes != WriterBatchBytes {
		t.Fatalf("writer limits = %d / %d", b.writer.BatchSize, b.writer.BatchBytes)
	}
	if b.maxCount != WriterBatchSize || b.maxBytes != WriterBatchBytes {
		t.Fatalf("bus split limits = %d / %d", b.maxCount, b.maxBytes)
	}
}

func TestPublishBatchSplitsToWriterBatches(t *testing.T) {
	f := &fakeWriter{respond: perKey(map[string]error{"d": kafka.NotLeaderForPartition, "e": kafka.NotLeaderForPartition})}
	b := testBus(f, time.Second)
	b.maxCount = 2
	errs, err := b.PublishBatch(t.Context(), envelopes("a", "b", "c", "d", "e"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || len(f.calls[0]) != 2 || len(f.calls[1]) != 2 || len(f.calls[2]) != 1 {
		t.Fatalf("calls = %v, want 2+2+1", f.calls)
	}
	for i, want := range []outcome{ok, ok, ok, retriable, retriable} {
		if got := classify(errs[i]); got != want {
			t.Fatalf("message %d: outcome %d (%v), want %d", i, got, errs[i], want)
		}
	}

	// By bytes: a cap of 2.5 messages fits two per call.
	raw, err := json.Marshal(envelopes("a")[0].Event)
	if err != nil {
		t.Fatal(err)
	}
	f = &fakeWriter{respond: perKey(nil)}
	b = testBus(f, time.Second)
	b.maxBytes = messageSize(kafka.Message{Key: []byte("a"), Value: raw}) * 5 / 2
	if _, err := b.PublishBatch(t.Context(), envelopes("a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || len(f.calls[0]) != 2 {
		t.Fatalf("calls = %v, want 2+1 by bytes", f.calls)
	}
}

// A chunk that cannot start in time is retried later without counting; the whole batch reads as a
// transient failure only when every chunk failed.
func TestPublishBatchSplitWholeFailure(t *testing.T) {
	f := &fakeWriter{respond: func(context.Context, []string, []kafka.Message) error { return kafka.NotEnoughReplicas }}
	b := testBus(f, time.Second)
	b.maxCount = 1
	errs, err := b.PublishBatch(t.Context(), envelopes("a", "b"))
	if err == nil || !errors.Is(err, kafka.NotEnoughReplicas) {
		t.Fatalf("err = %v (errs %v), want whole-batch transient failure", err, errs)
	}
}
