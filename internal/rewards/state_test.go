package rewards_test

import (
	"fmt"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

func TestRequestIDRing_RecordAndSee(t *testing.T) {
	var s rewards.CustomerState

	if s.SeenRequestID("req-1") {
		t.Error("empty ring claims to have seen req-1")
	}

	s.RecordRequestID("req-1")
	if !s.SeenRequestID("req-1") {
		t.Error("req-1 not seen after recording")
	}
	if s.SeenRequestID("req-2") {
		t.Error("req-2 seen without being recorded")
	}
}

// The empty ID is the opt-out: never recorded, never seen. Otherwise two
// callers that both omit requestId would dedupe against each other.
func TestRequestIDRing_EmptyIDOptsOut(t *testing.T) {
	var s rewards.CustomerState

	s.RecordRequestID("")
	if len(s.RecentRequestIDs) != 0 {
		t.Errorf("recording the empty ID grew the ring to %d", len(s.RecentRequestIDs))
	}
	if s.SeenRequestID("") {
		t.Error("the empty ID reports as seen")
	}
}

// The ring is bounded FIFO: once full, each record evicts the oldest entry.
func TestRequestIDRing_EvictsOldestAtCap(t *testing.T) {
	var s rewards.CustomerState
	total := rewards.RecentRequestIDCap + 5
	for i := 0; i < total; i++ {
		s.RecordRequestID(fmt.Sprintf("req-%d", i))
	}

	if len(s.RecentRequestIDs) != rewards.RecentRequestIDCap {
		t.Fatalf("ring holds %d entries, want the cap of %d",
			len(s.RecentRequestIDs), rewards.RecentRequestIDCap)
	}
	if s.SeenRequestID("req-4") {
		t.Error("req-4 still seen; the oldest entries should have been evicted")
	}
	if !s.SeenRequestID("req-5") {
		t.Error("req-5 not seen; eviction went past the overflow")
	}
	if !s.SeenRequestID(fmt.Sprintf("req-%d", total-1)) {
		t.Error("newest entry not seen")
	}
}
