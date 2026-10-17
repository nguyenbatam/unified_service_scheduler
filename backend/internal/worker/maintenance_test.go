package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPurgeAll(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		batches   []int64 // rows deleted by each call; past the end every call deletes a full batch
		failAt    int     // call index that fails (-1: none)
		wantCalls int
		wantTotal int64
		wantErr   error
	}{
		{"nothing to delete", []int64{0}, -1, 1, 0, nil},
		{"loops until a short batch", []int64{purgeBatch, purgeBatch, 10}, -1, 3, 2*purgeBatch + 10, nil},
		{"bounded per run", nil, -1, purgeMaxBatches, purgeMaxBatches * purgeBatch, nil},
		{"stops on error", []int64{purgeBatch, 0}, 1, 2, purgeBatch, boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			total, err := purgeAll(t.Context(), func(context.Context) (int64, error) {
				i := calls
				calls++
				if i == tt.failAt {
					return 0, boom
				}
				if i < len(tt.batches) {
					return tt.batches[i], nil
				}
				return purgeBatch, nil
			})
			if calls != tt.wantCalls || total != tt.wantTotal || !errors.Is(err, tt.wantErr) {
				t.Fatalf("calls=%d total=%d err=%v, want %d/%d/%v", calls, total, err, tt.wantCalls, tt.wantTotal, tt.wantErr)
			}
		})
	}
}

func TestPurgeAllRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	total, err := purgeAll(ctx, func(context.Context) (int64, error) {
		calls++
		cancel() // the run is cancelled while the first batch is deleting
		return purgeBatch, nil
	})
	if calls != 1 || total != purgeBatch || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d total=%d err=%v", calls, total, err)
	}
}

type panickingReminders struct{}

func (panickingReminders) EnqueueReminders(context.Context) (int, error) { panic("poisoned row") }

type countingSweeper struct{ noShows, overruns int }

func (s *countingSweeper) MarkNoShows(context.Context) (int, error) { s.noShows++; return 0, nil }

func (s *countingSweeper) HandleOverruns(context.Context, time.Duration) (int, int, error) {
	s.overruns++
	return 0, 0, nil
}

// A panicking task is logged and the tick goes on: the store is nil here, so both reclaims panic
// too, and the reminder sweep panics on purpose; the sweeps after them must still run.
func TestTickSurvivesPanickingTasks(t *testing.T) {
	var logs bytes.Buffer
	sweeper := &countingSweeper{}
	m := NewMaintenance(nil, sweeper, panickingReminders{}, slog.New(slog.NewTextHandler(&logs, nil)), DefaultMaintenanceConfig())
	m.Tick(t.Context())
	if sweeper.noShows != 1 || sweeper.overruns != 1 {
		t.Fatalf("sweeps after a panic ran %d/%d times, want 1/1", sweeper.noShows, sweeper.overruns)
	}
	for _, task := range []string{"outbox reclaim", "notification reclaim", "reminder sweep"} {
		if !strings.Contains(logs.String(), `task="`+task+`"`) {
			t.Errorf("no panic logged for %s:\n%s", task, logs.String())
		}
	}
	logs.Reset()
	m.Purge(t.Context()) // nil store: every purge panics and is recovered, and the next still runs
	for _, task := range []string{"outbox purge", "notification purge", "auth session purge"} {
		if !strings.Contains(logs.String(), `task="`+task+`"`) {
			t.Errorf("no panic logged for %s:\n%s", task, logs.String())
		}
	}
}
