package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/segmentio/kafka-go"
)

func TestReaderConfigStartOffset(t *testing.T) {
	brokers := []string{"localhost:9092"}
	if got := newConsumerConfig(brokers, "t", "scheduler-ws-1").reader.StartOffset; got != kafka.LastOffset {
		t.Fatalf("default start = %d, want LastOffset (per-instance fan-out skips history)", got)
	}
	if got := newConsumerConfig(brokers, "t", "scheduler-notify", StartFrom(FromEarliest)).reader.StartOffset; got != kafka.FirstOffset {
		t.Fatalf("shared group start = %d, want FirstOffset (no events lost before the first commit)", got)
	}
	if got := newConsumerConfig(brokers, "t", "g", StartFrom(FromLatest)).reader.StartOffset; got != kafka.LastOffset {
		t.Fatalf("explicit latest = %d", got)
	}
}

func TestReaderConfigWatchesPartitions(t *testing.T) {
	if !newConsumerConfig([]string{"localhost:9092"}, "t", "g").reader.WatchPartitionChanges {
		t.Fatal("a group that joined before the topic had partitions would never get any")
	}
}

// Only per-boot fan-out groups are deleted on close; the shared notify group must keep its offsets.
func TestConsumerDeleteGroupOnClose(t *testing.T) {
	brokers := []string{"localhost:9092"}
	if newConsumerConfig(brokers, "t", "scheduler-notify").deleteGroup {
		t.Fatal("work groups must survive a restart")
	}
	if !newConsumerConfig(brokers, "t", "ws", DeleteGroupOnClose(), StartFrom(FromLatest)).deleteGroup {
		t.Fatal("DeleteGroupOnClose must mark the group for deletion")
	}
}

// fakeReader hands out queued messages, then blocks until ctx ends; it records commits.
type fakeReader struct {
	msgs      chan kafka.Message
	mu        sync.Mutex
	committed []int64
	ctxErrs   []error // the commit context's state at each commit
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	select {
	case m := <-f.msgs:
		return m, nil
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	}
}

func (f *fakeReader) CommitMessages(ctx context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.committed = append(f.committed, m.Offset)
		f.ctxErrs = append(f.ctxErrs, ctx.Err())
	}
	return ctx.Err()
}

func (f *fakeReader) Close() error { return nil }

// A fan-out consumer commits what it handled: kafka-go re-reads committed offsets after every
// rebalance, and with none it would restart at the end of the topic and drop events (including
// SESSION_REVOKED). Undecodable messages are committed too (skipped, not retried forever).
func TestConsumerCommitsHandledMessages(t *testing.T) {
	r := &fakeReader{msgs: make(chan kafka.Message, 3)}
	r.msgs <- kafka.Message{Offset: 1, Value: []byte(`{"type":1}`)}
	r.msgs <- kafka.Message{Offset: 2, Value: []byte(`not json`)}
	r.msgs <- kafka.Message{Offset: 3, Value: []byte(`{"type":12}`)}
	var handled atomic.Int32
	c := &Consumer{
		reader: r, name: "scheduler-ws-test", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		handle: func(context.Context, realtime.Event) error { handled.Add(1); return nil },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.committed)
		r.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("committed %d of 3 messages", n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if handled.Load() != 2 {
		t.Fatalf("handled = %d, want 2", handled.Load())
	}
	if !slices.Equal(r.committed, []int64{1, 2, 3}) {
		t.Fatalf("committed = %v", r.committed)
	}
}

// Shutdown that lands while a message is being handled must not drop its commit: the handler
// finished, so the offset is committed with a context shutdown does not cancel (kafka-go's
// CommitMessages gives up at once on a cancelled one, and the message would be redelivered).
func TestConsumerCommitsMessageHandledDuringShutdown(t *testing.T) {
	r := &fakeReader{msgs: make(chan kafka.Message, 1)}
	r.msgs <- kafka.Message{Offset: 7, Value: []byte(`{"type":1}`)}
	ctx, cancel := context.WithCancel(t.Context())
	c := &Consumer{
		reader: r, name: "scheduler-notify", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		handle: func(context.Context, realtime.Event) error { cancel(); return nil }, // shutdown mid-handle
	}
	c.Run(ctx) // returns once FetchMessage sees the cancelled context
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(r.committed, []int64{7}) || r.ctxErrs[0] != nil {
		t.Fatalf("committed %v with context errors %v, want offset 7 committed on a live context", r.committed, r.ctxErrs)
	}
}

// Every boot gets a fresh fan-out group, so a restart never resumes from an older run's offsets.
func TestFanoutGroupIDIsPerBoot(t *testing.T) {
	a, b := FanoutGroupID("backend-1"), FanoutGroupID("backend-1")
	if a == b {
		t.Fatalf("two boots share group %q", a)
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "scheduler-ws-backend-1-") {
			t.Fatalf("group %q must name the instance", id)
		}
	}
}

func TestChainRunsEveryHandler(t *testing.T) {
	var calls []string
	boom := errors.New("boom")
	h := Chain(
		func(context.Context, realtime.Event) error { calls = append(calls, "a"); return boom },
		func(context.Context, realtime.Event) error { calls = append(calls, "b"); return nil },
	)
	if err := h(t.Context(), realtime.Event{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if strings.Join(calls, ",") != "a,b" {
		t.Fatalf("calls = %v: a failing handler must not starve the next", calls)
	}
}

// With no broker reachable the retry loop keeps going (the outbox needs the topic eventually) and
// still returns as soon as ctx ends.
func TestKeepEnsuringTopicStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		KeepEnsuringTopic(ctx, []string{"127.0.0.1:1"}, "t", 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("returned before the topic existed or ctx ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop after ctx ended")
	}
}

// waitCommitted waits until r has committed n messages.
func waitCommitted(t *testing.T, r *fakeReader, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r.mu.Lock()
		got := len(r.committed)
		r.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("committed %d of %d messages", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A handler that panics on one event must not crash the process (every replica would read the
// same message after a restart and crash again) nor stall the partition forever: the panic is
// recovered, retried a few times like an error, then the event is skipped and committed, and the
// next event is handled normally.
func TestConsumerRecoversFromPanickingHandler(t *testing.T) {
	r := &fakeReader{msgs: make(chan kafka.Message, 2)}
	r.msgs <- kafka.Message{Offset: 1, Value: []byte(`{"type":99}`)}
	r.msgs <- kafka.Message{Offset: 2, Value: []byte(`{"type":1}`)}
	var poisonCalls, handled atomic.Int32
	c := &Consumer{
		reader: r, name: "scheduler-notify", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		handle: func(_ context.Context, ev realtime.Event) error {
			if ev.Type == domain.EventType(99) {
				poisonCalls.Add(1)
				panic(errors.New("poisoned event"))
			}
			handled.Add(1)
			return nil
		},
	}
	skippedBefore := SkippedEvents()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitCommitted(t, r, 2, 15*time.Second) // maxHandlerPanics tries with doubling backoff: ~4 s
	cancel()
	<-done
	if SkippedEvents() <= skippedBefore {
		t.Fatal("a skipped event was not counted (scheduler_events_skipped_total)")
	}
	if poisonCalls.Load() != maxHandlerPanics || handled.Load() != 1 {
		t.Fatalf("poison tried %d times (want %d), handled %d (want 1)", poisonCalls.Load(), maxHandlerPanics, handled.Load())
	}
	if !slices.Equal(r.committed, []int64{1, 2}) {
		t.Fatalf("committed = %v", r.committed)
	}
}

// A panic in one chain member is reported as a *PanicError and the members after it still run.
func TestChainRecoversPanickingMember(t *testing.T) {
	ran := false
	h := Chain(
		func(context.Context, realtime.Event) error { panic("boom") },
		func(context.Context, realtime.Event) error { ran = true; return nil },
	)
	err := h(t.Context(), realtime.Event{})
	if pe, ok := errors.AsType[*PanicError](err); !ok || pe.Value != "boom" || len(pe.Stack) == 0 {
		t.Fatalf("err = %v, want a *PanicError for boom", err)
	}
	if !ran {
		t.Fatal("a panicking member must not starve the next")
	}
}

// The fan-out consumer never decodes the PII payload; the routing envelope is intact. Without
// SkipPayload (the notifier) the payload is kept.
func TestConsumerSkipPayload(t *testing.T) {
	raw := []byte(`{"id":"0b6f8c1e-7f1d-4c51-9a37-2a2f5f4b9d10","type":1,"kind":1,` +
		`"customerId":"c1000000-0000-4000-8000-000000000001","appointment":{"version":3},` +
		`"payload":{"customerName":"Amelia Clarke","vin":"WBA00000000000001"}}`)
	fanout := &Consumer{skipPayload: newConsumerConfig(nil, "t", "ws", SkipPayload()).skipPayload}
	ev, err := fanout.decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Payload != nil {
		t.Fatalf("payload decoded: %s", ev.Payload)
	}
	if ev.Type != domain.EventAppointmentCreated || ev.Kind != realtime.KindAppointment || ev.ID.String() != "0b6f8c1e-7f1d-4c51-9a37-2a2f5f4b9d10" ||
		ev.CustomerID.String() != "c1000000-0000-4000-8000-000000000001" || ev.Appointment == nil || ev.Appointment.Version != 3 {
		t.Fatalf("envelope = %+v", ev)
	}
	if _, err := fanout.decode([]byte(`{"type":"X","payload":`)); err == nil {
		t.Fatal("truncated message must still fail to decode")
	}
	full, err := (&Consumer{}).decode(raw)
	if err != nil || !strings.Contains(string(full.Payload), "Amelia") {
		t.Fatalf("notifier payload = %s, %v", full.Payload, err)
	}
}

// A chain whose failure includes an ordinary error is retried like an error even if another member
// panicked: only an all-panic failure counts toward maxHandlerPanics (and may be skipped).
func TestOnlyPanics(t *testing.T) {
	pe := &PanicError{Value: "boom"}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"panic", pe, true},
		{"joined panics", errors.Join(pe, &PanicError{Value: "again"}), true},
		{"panic and error", errors.Join(pe, errors.New("db down")), false},
		{"error", errors.New("db down"), false},
	} {
		if got, ok := onlyPanics(tt.err); ok != tt.want || (ok && got != pe) {
			t.Errorf("%s: onlyPanics = %v, %v; want %v", tt.name, got, ok, tt.want)
		}
	}
}
