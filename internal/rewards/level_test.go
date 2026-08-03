package rewards_test

import (
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// Tier derivation is a pure function, so no test environment is needed --
// the payoff of keeping the rules out of the workflow package.

// Level and PrevTierAt lean on the bottom rung covering every balance; a
// ladder that starts above 0 would leave low balances standing on nothing.
func TestLadderStartsAtBasic(t *testing.T) {
	ladder := rewards.Ladder()
	if len(ladder) == 0 {
		t.Fatal("Ladder() is empty")
	}
	if ladder[0].Level != rewards.LevelBasic || ladder[0].MinPoints != 0 {
		t.Errorf("Ladder() starts with %+v, want {%s 0}", ladder[0], rewards.LevelBasic)
	}
}

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

func TestPrevTierAt(t *testing.T) {
	for _, tc := range []struct {
		points int
		want   int
	}{
		{0, 0},
		{499, 0},
		{500, rewards.GoldThreshold},
		{999, rewards.GoldThreshold},
		{1000, rewards.PlatinumThreshold},
		{50000, rewards.PlatinumThreshold},
	} {
		if got := rewards.PrevTierAt(tc.points); got != tc.want {
			t.Errorf("PrevTierAt(%d) = %d, want %d", tc.points, got, tc.want)
		}
	}
}
