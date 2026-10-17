package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/segmentio/kafka-go"
)

// Envelope is one event plus its partition key.
type Envelope struct {
	Key   string
	Event realtime.Event
}

// ErrOutcomeUnknown marks a write abandoned before kafka-go finished it (the call's context
// ended): the messages may still land, up to MaxWriteLifetime later.
var ErrOutcomeUnknown = errors.New("publish outcome unknown")

// errNoTime: the caller's deadline is too close to start a write whose batch is guaranteed to be
// finished by then. Nothing was sent.
var errNoTime = errors.New("publish deadline too close to start a kafka write")

// RetriableError wraps a per-message failure that says nothing about the message itself (leader
// election, under-replicated partition, request timeout, a batch cut short): retry it without
// counting a rejection. Any other per-message error is a rejection of that message.
type RetriableError struct{ Err error }

func (e *RetriableError) Error() string { return e.Err.Error() }
func (e *RetriableError) Unwrap() error { return e.Err }

// IsRetriable reports whether a per-message publish error is transient (see RetriableError).
func IsRetriable(err error) bool {
	_, ok := errors.AsType[*RetriableError](err)
	return ok
}

// IsOutcomeUnknown reports whether the messages behind err may still be written later.
func IsOutcomeUnknown(err error) bool { return errors.Is(err, ErrOutcomeUnknown) }

// isRejection reports whether the broker (or kafka-go, before sending) refused the message for
// what it contains, so resending it can never succeed. Everything else, including non-temporary
// cluster errors such as authorization or version mismatches, says nothing about one message and
// is retried without counting: it would hit every message alike, so counting it would poison the
// whole outbox during a misconfiguration.
func isRejection(err error) bool {
	if _, ok := errors.AsType[kafka.MessageTooLargeError](err); ok {
		return true
	}
	kerr, ok := errors.AsType[kafka.Error](err)
	if !ok {
		return false
	}
	switch kerr {
	case kafka.MessageSizeTooLarge, kafka.RecordListTooLarge, kafka.InvalidMessageSize, kafka.InvalidRecord,
		// CorruptMessage: kafka-go retries it as temporary; still failing after every attempt
		// means this record is the problem.
		kafka.InvalidMessage:
		return true
	}
	return false
}

// PublishBatch writes all envelopes, normally in one call. It returns a per-envelope error slice
// (nil entries succeeded) so the caller can retry only what failed: a *RetriableError entry is
// transient and must not count against the message; any other entry is a rejection of that
// message. err is set for whole-batch failures (nothing per message can be said, or every message
// failed transiently), in which case every envelope must be retried; IsOutcomeUnknown(err) means
// the batch may still land.
//
// kafka-go fails a partition batch as a whole, so a content rejection reported for several
// messages cannot be pinned on one of them; those are resent one by one to find the culprit.
// A write only starts when ctx leaves at least MaxWriteLifetime: kafka-go keeps a queued batch
// alive past ctx, and the caller's deadline must stay an upper bound on what it still has in flight
// (plus MaxWriteLifetime for an abandoned call).
func (b *Bus) PublishBatch(ctx context.Context, batch []Envelope) ([]error, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	errs := make([]error, len(batch))
	msgs := make([]kafka.Message, len(batch))
	idx := make([]int, 0, len(batch))
	now := time.Now().UTC()
	for i, env := range batch {
		raw, err := json.Marshal(env.Event)
		if err != nil {
			errs[i] = err // cannot be encoded: a rejection, never sent
			continue
		}
		key := env.Key
		if key == "" {
			key = env.Event.Type.Name()
		}
		msgs[i] = kafka.Message{Key: []byte(key), Value: raw, Time: now}
		idx = append(idx, i)
	}
	if len(idx) > 0 {
		if err := b.send(ctx, msgs, idx, errs); err != nil {
			return nil, err
		}
	}
	return errs, wholeBatch(errs)
}

// wholeBatch returns the batch's error when every message failed transiently (a cluster-wide
// problem: the caller backs off as for an outage), preferring one whose outcome is unknown.
func wholeBatch(errs []error) error {
	var first error
	for _, err := range errs {
		if err == nil || !IsRetriable(err) {
			return nil
		}
		if first == nil || (IsOutcomeUnknown(err) && !IsOutcomeUnknown(first)) {
			first = err
		}
	}
	return first
}

// send writes msgs[idx] in one call and records each message's outcome in errs. It returns an
// error only for a whole-call failure, when nothing can be said per message. More than one
// writer batch's worth of messages is sent in consecutive calls (see chunks); a chunk's whole-call
// failure is then recorded as retriable on each of its messages.
func (b *Bus) send(ctx context.Context, msgs []kafka.Message, idx []int, errs []error) error {
	if parts := b.chunks(msgs, idx); len(parts) > 1 {
		for _, part := range parts {
			if err := b.send(ctx, msgs, part, errs); err != nil {
				for _, i := range part {
					errs[i] = &RetriableError{Err: err}
				}
			}
		}
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.Sub(b.clock()) < b.lifetime {
		return errNoTime
	}
	sub := make([]kafka.Message, len(idx))
	for j, i := range idx {
		sub[j] = msgs[i]
	}
	// Normally the call returns once kafka-go is done with every batch (<= lifetime); the cap only
	// matters when the metadata lookup before queueing is slow.
	callCtx, cancel := context.WithTimeout(ctx, b.lifetime)
	defer cancel()
	err := b.write(callCtx, sub...)
	if err == nil {
		return nil
	}

	if tooLarge, ok := errors.AsType[kafka.MessageTooLargeError](err); ok {
		// kafka-go refused the call before sending anything: one message exceeds BatchBytes.
		// Reject it and send the rest again.
		j := slices.IndexFunc(idx, func(i int) bool {
			return bytes.Equal(msgs[i].Value, tooLarge.Message.Value) && bytes.Equal(msgs[i].Key, tooLarge.Message.Key)
		})
		if j < 0 {
			return err
		}
		errs[idx[j]] = err
		rest := slices.Delete(slices.Clone(idx), j, j+1)
		if len(rest) > 0 {
			if err := b.send(ctx, msgs, rest, errs); err != nil {
				for _, i := range rest {
					errs[i] = &RetriableError{Err: err}
				}
			}
		}
		return nil
	}

	perMessage, ok := errors.AsType[kafka.WriteErrors](err)
	if !ok || len(perMessage) != len(idx) {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		}
		return err
	}
	var suspects []int // indexes into msgs
	for j, i := range idx {
		switch msgErr := perMessage[j]; {
		case msgErr == nil:
		case isRejection(msgErr):
			suspects = append(suspects, i)
			errs[i] = msgErr
		default:
			errs[i] = &RetriableError{Err: msgErr}
		}
	}
	if len(suspects) <= 1 {
		// A lone content error is unambiguous: its batch-mates would carry the same error.
		return nil
	}
	for _, i := range suspects {
		orig := errs[i]
		errs[i] = nil
		if err := b.send(ctx, msgs, []int{i}, errs); err != nil {
			errs[i] = &RetriableError{Err: fmt.Errorf("%w; resent alone: %w", orig, err)}
		}
	}
	return nil
}

func (b *Bus) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// chunks splits idx into runs that each fit one kafka-go batch (maxCount messages, maxBytes) even
// if they all hash to one partition, so a call never queues a second batch behind the first and
// MaxWriteLifetime holds. Batches hold at most one event per aggregate (one key), so sending them
// one after another cannot reorder an aggregate. A single message over maxBytes gets a run of its
// own (kafka-go rejects it with MessageTooLargeError).
func (b *Bus) chunks(msgs []kafka.Message, idx []int) [][]int {
	var out [][]int
	start, total := 0, int64(0)
	for j, i := range idx {
		size := messageSize(msgs[i])
		if j > start && ((b.maxCount > 0 && j-start >= b.maxCount) || (b.maxBytes > 0 && total+size > b.maxBytes)) {
			out = append(out, idx[start:j])
			start, total = j, 0
		}
		total += size
	}
	return append(out, idx[start:])
}

// messageSize is the size kafka-go charges a header-less message against BatchBytes (its
// Message.totalSize: CRC, magic byte, attributes, timestamp, length-prefixed key and value, and a
// one-byte empty header count).
func messageSize(m kafka.Message) int64 {
	return int64(4+1+1+8+4+len(m.Key)+4+len(m.Value)) + 1
}
