package rewards_test

import (
	"fmt"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

func TestPriorResult(t *testing.T) {
	var s rewards.CustomerState
	if _, ok := s.PriorResult("req-1"); ok {
		t.Fatal("empty ledger should miss")
	}

	s.RecordApplied("req-1", rewards.AddPointsResult{Balance: 100, Level: rewards.LevelBasic})
	got, ok := s.PriorResult("req-1")
	if !ok {
		t.Fatal("recorded request should hit")
	}
	if got.Balance != 100 || got.Level != rewards.LevelBasic {
		t.Errorf("got %+v, want balance 100, level basic", got)
	}
	if _, ok := s.PriorResult("req-2"); ok {
		t.Error("unknown request should miss")
	}
}

func TestRecordAppliedEvictsOldest(t *testing.T) {
	var s rewards.CustomerState
	for i := 0; i < rewards.RecentRequestsCap+2; i++ {
		s.RecordApplied(fmt.Sprintf("req-%d", i), rewards.AddPointsResult{Balance: i})
	}

	if n := len(s.RecentRequests); n != rewards.RecentRequestsCap {
		t.Fatalf("ledger length = %d, want %d", n, rewards.RecentRequestsCap)
	}
	if _, ok := s.PriorResult("req-0"); ok {
		t.Error("oldest entry should have been evicted")
	}
	if _, ok := s.PriorResult("req-1"); ok {
		t.Error("second-oldest entry should have been evicted")
	}
	newest := fmt.Sprintf("req-%d", rewards.RecentRequestsCap+1)
	if got, ok := s.PriorResult(newest); !ok || got.Balance != rewards.RecentRequestsCap+1 {
		t.Errorf("newest entry missing or wrong: %+v (ok=%v)", got, ok)
	}
}
