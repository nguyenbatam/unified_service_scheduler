package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The oracle's calendar on New York's clock-change Sundays for a shop open 00:30–06:00 (the change
// at 02:00 falls inside the opening hours): starts are 30 elapsed minutes apart, so the skipped
// hour has none and the repeated hour has two of each wall clock, as distinct instants; the count
// follows the day's real length; a 30-minute staging window of the first 01:00 is not the second's.
func TestOracleStartsOnClockChangeDays(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	w := workshop{loc: ny, dealer: domain.Dealership{OpenTime: "00:30", CloseTime: "06:00", LunchStart: "03:00", LunchEnd: "03:30"}}
	cases := []struct {
		name                  string
		day                   booking.Date
		job30, job60          int // starts for a 30- and a 60-minute job
		wantWall              []string
		lunchFree, lunchTouch int // 30-minute starts clear of lunch / touching it
	}{
		// 00:30, 01:00, 01:30, then 03:00 (EDT) …: 4.5 h of real time.
		{"spring forward", booking.Date{Year: 2030, Month: time.March, Day: 10}, 9, 8,
			[]string{"00:30", "01:00", "01:30", "03:00", "03:30", "04:00", "04:30", "05:00", "05:30"}, 8, 1},
		{"ordinary Sunday", booking.Date{Year: 2030, Month: time.March, Day: 17}, 11, 10,
			[]string{"00:30", "01:00", "01:30", "02:00", "02:30", "03:00", "03:30", "04:00", "04:30", "05:00", "05:30"}, 10, 1},
		// 01:00 and 01:30 twice (EDT, then EST): 6.5 h of real time.
		{"fall back", booking.Date{Year: 2030, Month: time.November, Day: 3}, 13, 12,
			[]string{"00:30", "01:00", "01:30", "01:00", "01:30", "02:00", "02:30", "03:00", "03:30", "04:00", "04:30", "05:00", "05:30"}, 12, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slots, err := dayStarts(w, tc.day, 30)
			if err != nil {
				t.Fatal(err)
			}
			long, err := dayStarts(w, tc.day, 60)
			if err != nil {
				t.Fatal(err)
			}
			if len(slots) != tc.job30 || len(long) != tc.job60 {
				t.Fatalf("%d starts for 30 min, %d for 60 min; want %d and %d", len(slots), len(long), tc.job30, tc.job60)
			}
			seen := map[int64]bool{}
			free, touch := 0, 0
			for i, s := range slots {
				if seen[s.start.Unix()] {
					t.Fatalf("start %s twice", s.start)
				}
				seen[s.start.Unix()] = true
				if got := s.start.In(ny).Format("15:04"); got != tc.wantWall[i] {
					t.Fatalf("start %d is %s local, want %s", i, got, tc.wantWall[i])
				}
				if i > 0 && s.start.Sub(slots[i-1].start) != slotStep {
					t.Fatalf("start %d is %s after the previous one", i, s.start.Sub(slots[i-1].start))
				}
				if s.lunch {
					touch++
				} else {
					free++
				}
			}
			if free != tc.lunchFree || touch != tc.lunchTouch {
				t.Fatalf("%d starts clear of lunch, %d touching it; want %d and %d", free, touch, tc.lunchFree, tc.lunchTouch)
			}
			if last := slots[len(slots)-1].start.Add(30 * time.Minute); !last.Equal(time.Date(tc.day.Year, tc.day.Month, tc.day.Day, 6, 0, 0, 0, ny)) {
				t.Fatalf("the last 30-minute job ends at %s, not at closing", last)
			}
		})
	}
	// The repeated hour: 01:00 EDT and 01:00 EST start two staging windows, 01:15 EST is in the second.
	firstOne := time.Date(2030, time.November, 3, 5, 0, 0, 0, time.UTC) // 01:00 EDT
	secondOne := firstOne.Add(time.Hour)                                // 01:00 EST
	if a, b := arrivalWindowStart(firstOne, ny, 30), arrivalWindowStart(secondOne, ny, 30); a.Equal(b) || !a.Equal(firstOne) || !b.Equal(secondOne) {
		t.Fatalf("windows of the two 01:00s: %s and %s", a, b)
	}
	if got := arrivalWindowStart(secondOne.Add(15*time.Minute), ny, 30); !got.Equal(secondOne) {
		t.Fatalf("01:15 EST is in the window of %s", got)
	}
	shop := &oracleShop{loc: ny, stagingCap: 1, stagingWindow: 30, holds: []oHold{{id: uuid.New(), status: 1, window: timeWindow{firstOne, firstOne.Add(time.Hour)}}}}
	if shop.stagingRoom(firstOne.Add(10*time.Minute), uuid.Nil) || !shop.stagingRoom(secondOne, uuid.Nil) {
		t.Fatal("the first 01:00 window is full, the second (an hour later) has room")
	}
}
