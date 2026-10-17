package main

import (
	"math"
	"slices"
	"testing"
)

func TestSoakGrowth(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
		ok     bool
	}{
		{"flat", []float64{100, 101, 99, 100, 102, 100, 99, 101}, 0, true},
		{"leaking", []float64{100, 100, 110, 120, 130, 140, 150, 160}, 0.6, true},
		{"one spike in the tail is not growth", []float64{100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 300, 100}, 0, true},
		{"too few samples", []float64{100, 200, 300}, 0, false},
	}
	for _, tc := range cases {
		got, ok := soakGrowth(tc.values)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s: soakGrowth = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// Every instance seen in any sample is judged on its own samples: one whose first scrape failed
// is not skipped, one that dropped out after a few samples fails as too few (no panic), and a
// backend never sampled at all fails the verdict.
func TestSoakMemoryPerInstance(t *testing.T) {
	flat := func(a, b float64) soakSample {
		return soakSample{rss: map[string]float64{"a": a, "b": b}, goroutines: map[string]float64{"a": 40, "b": 40}}
	}
	onlyA := soakSample{rss: map[string]float64{"a": 100}, goroutines: map[string]float64{"a": 40}}
	var samples []soakSample
	samples = append(samples, onlyA) // b's first scrape failed
	for range 8 {
		samples = append(samples, flat(100, 100))
	}
	if parts, ok := soakMemory(samples, 2, 20); !ok || len(parts) != 2 {
		t.Fatalf("both instances flat: ok=%v parts=%v, want ok with a and b judged", ok, parts)
	}
	grown := slices.Clone(samples)
	for i := 5; i < len(grown); i++ {
		grown[i] = flat(100, 200)
	}
	if _, ok := soakMemory(grown, 2, 20); ok {
		t.Fatal("b doubled its resident memory: want a failure even though its first scrape failed")
	}
	dropped := []soakSample{flat(100, 100), flat(100, 100), onlyA, onlyA, onlyA, onlyA, onlyA, onlyA}
	if parts, ok := soakMemory(dropped, 2, 20); ok {
		t.Fatalf("b answered once after warm-up: want too few samples, got ok (%v)", parts)
	}
	if _, ok := soakMemory([]soakSample{onlyA, onlyA, onlyA, onlyA, onlyA, onlyA}, 2, 20); ok {
		t.Fatal("b never answered: want a failure")
	}
}
