package rewards_test

import (
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// --- Tier derivation (FINDINGS.md#tiers-are-derived-never-stored) ----------
//
// Pure-function boundaries. Cheap to assert exhaustively, and these are the
// numbers a stakeholder will ask about. They need no test environment, which is
// the practical payoff of keeping the rules out of the workflow package.

func TestLevelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		points int
		want   string
	}{
		{0, rewards.LevelBasic},
		{1, rewards.LevelBasic},
		{499, rewards.LevelBasic},
		{500, rewards.LevelGold},
		{501, rewards.LevelGold},
		{999, rewards.LevelGold},
		{1000, rewards.LevelPlatinum},
		{50000, rewards.LevelPlatinum},
	} {
		if got := rewards.Level(tc.points); got != tc.want {
			t.Errorf("Level(%d) = %q, want %q", tc.points, got, tc.want)
		}
	}
}

func TestNextTierAt(t *testing.T) {
	for _, tc := range []struct {
		points  int
		wantAt  int
		wantHas bool
	}{
		{0, rewards.GoldThreshold, true},
		{499, rewards.GoldThreshold, true},
		{500, rewards.PlatinumThreshold, true},
		{999, rewards.PlatinumThreshold, true},
		{1000, 0, false},
	} {
		gotAt, gotHas := rewards.NextTierAt(tc.points)
		if gotAt != tc.wantAt || gotHas != tc.wantHas {
			t.Errorf("NextTierAt(%d) = (%d, %v), want (%d, %v)",
				tc.points, gotAt, gotHas, tc.wantAt, tc.wantHas)
		}
	}
}
