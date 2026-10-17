package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/segmentio/kafka-go"
)

// Transient per-message errors only back off; only what the bus did not mark retriable counts as
// a rejection, and a possibly-landed write is held back past the writer lifetime.
func TestSortOutcomes(t *testing.T) {
	perMessage := []error{
		nil,
		&events.RetriableError{Err: kafka.NotEnoughReplicas},
		kafka.MessageSizeTooLarge,
		&events.RetriableError{Err: fmt.Errorf("%w: %w", events.ErrOutcomeUnknown, context.DeadlineExceeded)},
		context.DeadlineExceeded,                              // bare, from some other publisher: cut short, maybe sent
		&events.RetriableError{Err: context.DeadlineExceeded}, // kafka-go's own request timeout: batch done
	}
	out := sortOutcomes(perMessage)
	for name, got := range map[string][]int{"published": out.published, "rejected": out.rejected, "retry": out.retry, "unknown": out.unknown} {
		want := map[string][]int{"published": {0}, "rejected": {2}, "retry": {1, 5}, "unknown": {3, 4}}[name]
		if !slices.Equal(got, want) {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
	if !errors.Is(out.firstErr, kafka.NotEnoughReplicas) {
		t.Fatalf("firstErr = %v", out.firstErr)
	}
}

func TestUnknownDelay(t *testing.T) {
	if d := unknownDelay(&events.RetriableError{Err: fmt.Errorf("%w: x", events.ErrOutcomeUnknown)}); d != events.MaxWriteLifetime {
		t.Fatalf("unknown outcome delay = %s", d)
	}
	if d := unknownDelay(&events.RetriableError{Err: kafka.NotEnoughReplicas}); d != 0 {
		t.Fatalf("certain failure delay = %s", d)
	}
}

func TestOutboxConfigValidate(t *testing.T) {
	lease := DefaultMaintenanceConfig().Lease
	if err := DefaultOutboxConfig().Validate(lease); err != nil {
		t.Fatalf("defaults must fit the default lease: %v", err)
	}
	short := DefaultOutboxConfig()
	short.PublishTimeout = events.MaxWriteLifetime // no room to isolate a poison message
	if short.Validate(lease) == nil {
		t.Fatal("publish timeout below twice the writer lifetime must be refused")
	}
	short.PublishTimeout = 2 * events.MaxWriteLifetime // the claim runs inside the deadline too
	if short.Validate(lease) == nil {
		t.Fatal("publish timeout must leave the claim its timeout on top of two writer lifetimes")
	}
	short.PublishTimeout = claimTimeout + 2*events.MaxWriteLifetime
	if err := short.Validate(lease); err != nil {
		t.Fatalf("claim + two writer lifetimes fits: %v", err)
	}
	long := DefaultOutboxConfig()
	long.PublishTimeout = lease - events.MaxWriteLifetime
	if long.Validate(lease) == nil {
		t.Fatal("an abandoned batch could outlive the lease")
	}
	if DefaultOutboxConfig().Validate(10*time.Second) == nil {
		t.Fatal("a lease shorter than the publish deadline must be refused")
	}
	big := DefaultOutboxConfig()
	big.BatchSize = events.WriterBatchSize + 1 // a claim could queue two batches for one partition
	if big.Validate(lease) == nil {
		t.Fatal("a batch larger than the kafka writer batch must be refused")
	}
	big.BatchSize = events.WriterBatchSize
	if err := big.Validate(lease); err != nil {
		t.Fatalf("a batch of exactly the writer batch size fits: %v", err)
	}
}

func TestOutboxDrainTimeoutCoversFlush(t *testing.T) {
	cfg := DefaultOutboxConfig()
	if got := cfg.DrainTimeout(); got != cfg.PublishTimeout+bookkeepingTimeout {
		t.Fatalf("drain timeout = %s", got)
	}
}

// A LISTEN session that keeps failing at once backs off up to 30s; one that ran healthily for a
// while starts again at 1s, whatever an earlier outage had grown the backoff to.
func TestListenBackoff(t *testing.T) {
	var got []time.Duration
	b := time.Duration(0)
	for range 7 {
		b = listenBackoff(b, 10*time.Millisecond)
		got = append(got, b)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff while failing fast = %v, want %v", got, want)
	}
	if b := listenBackoff(30*time.Second, listenHealthy); b != time.Second {
		t.Fatalf("backoff after a healthy session = %s, want 1s", b)
	}
	if b := listenBackoff(30*time.Second, listenHealthy-time.Second); b != 30*time.Second {
		t.Fatalf("backoff after a short session = %s, want it kept at 30s", b)
	}
}
