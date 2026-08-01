package rewards_test

import (
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// --- Tier derivation (FINDINGS.md#tiers-are-derived-never-stored) ----------
//
// Pure-function boundaries, asserted exhaustively. No test environment needed,
// which is the payoff of keeping the rules out of the workflow package.
//
// Both ladders, because both are live: a run that predates the
// rewards.ChangeTierThresholds marker walks TiersV1 until it continues as new,
// and one started today walks TiersV2. The 450--499 band is the whole point of
// the versioning, so it appears in every table below.

func TestLadderBoundaries(t *testing.T) {
	for _, tc := range []struct {
		points int
		v1     string
		v2     string
	}{
		{0, rewards.LevelBasic, rewards.LevelBasic},
		{1, rewards.LevelBasic, rewards.LevelBasic},
		{449, rewards.LevelBasic, rewards.LevelBasic},
		{450, rewards.LevelBasic, rewards.LevelGold},
		{499, rewards.LevelBasic, rewards.LevelGold},
		{500, rewards.LevelGold, rewards.LevelGold},
		{949, rewards.LevelGold, rewards.LevelGold},
		{950, rewards.LevelGold, rewards.LevelPlatinum},
		{999, rewards.LevelGold, rewards.LevelPlatinum},
		{1000, rewards.LevelPlatinum, rewards.LevelPlatinum},
		{50000, rewards.LevelPlatinum, rewards.LevelPlatinum},
	} {
		if got := rewards.TiersV1.Level(tc.points); got != tc.v1 {
			t.Errorf("TiersV1.Level(%d) = %q, want %q", tc.points, got, tc.v1)
		}
		if got := rewards.TiersV2.Level(tc.points); got != tc.v2 {
			t.Errorf("TiersV2.Level(%d) = %q, want %q", tc.points, got, tc.v2)
		}
	}
}

func TestNextTierAt(t *testing.T) {
	for _, tc := range []struct {
		points int
		v1     int // 0 means "already at the top tier"
		v2     int
	}{
		{0, rewards.GoldThreshold, rewards.GoldThresholdV2},
		{449, rewards.GoldThreshold, rewards.GoldThresholdV2},
		{450, rewards.GoldThreshold, rewards.PlatinumThresholdV2},
		{499, rewards.GoldThreshold, rewards.PlatinumThresholdV2},
		{500, rewards.PlatinumThreshold, rewards.PlatinumThresholdV2},
		{949, rewards.PlatinumThreshold, rewards.PlatinumThresholdV2},
		{950, rewards.PlatinumThreshold, 0},
		{999, rewards.PlatinumThreshold, 0},
		{1000, 0, 0},
	} {
		for _, l := range []struct {
			name   string
			ladder rewards.TierLadder
			want   int
		}{
			{"TiersV1", rewards.TiersV1, tc.v1},
			{"TiersV2", rewards.TiersV2, tc.v2},
		} {
			gotAt, gotHas := l.ladder.NextTierAt(tc.points)
			if gotAt != l.want || gotHas != (l.want != 0) {
				t.Errorf("%s.NextTierAt(%d) = (%d, %v), want (%d, %v)",
					l.name, tc.points, gotAt, gotHas, l.want, l.want != 0)
			}
		}
	}
}

// TierFloor is the other end of the rung the progress bar draws, so it has to
// agree with Level: whatever tier Level names, TierFloor is the balance that
// earned it.
func TestTierFloor(t *testing.T) {
	for _, tc := range []struct {
		points int
		v1     int
		v2     int
	}{
		{0, 0, 0},
		{449, 0, 0},
		{450, 0, rewards.GoldThresholdV2},
		{499, 0, rewards.GoldThresholdV2},
		{500, rewards.GoldThreshold, rewards.GoldThresholdV2},
		{949, rewards.GoldThreshold, rewards.GoldThresholdV2},
		{950, rewards.GoldThreshold, rewards.PlatinumThresholdV2},
		{1000, rewards.PlatinumThreshold, rewards.PlatinumThresholdV2},
	} {
		if got := rewards.TiersV1.TierFloor(tc.points); got != tc.v1 {
			t.Errorf("TiersV1.TierFloor(%d) = %d, want %d", tc.points, got, tc.v1)
		}
		if got := rewards.TiersV2.TierFloor(tc.points); got != tc.v2 {
			t.Errorf("TiersV2.TierFloor(%d) = %d, want %d", tc.points, got, tc.v2)
		}
	}
}

// The ladder a caller outside the workflow resolves from a run's
// TemporalChangeVersion, which has to match what the run itself decided or the
// API advertises a promotion the workflow will not make.
//
// Absent marker means DefaultVersion means v1, and that is the case worth
// pinning: it is what every history recorded before this change looks like.
func TestTiersForChangeVersions(t *testing.T) {
	for _, tc := range []struct {
		what     string
		versions []string
		want     rewards.TierLadder
	}{
		{"no attribute at all (pre-marker run)", nil, rewards.TiersV1},
		{"attribute present but empty", []string{}, rewards.TiersV1},
		{"some other change entirely", []string{"tier-notifications-1"}, rewards.TiersV1},
		{"the marker", []string{rewards.ChangeVersionTierThresholds}, rewards.TiersV2},
		{"the marker among others", []string{
			"tier-notifications-1", rewards.ChangeVersionTierThresholds,
		}, rewards.TiersV2},
	} {
		got := rewards.TiersForChangeVersions(tc.versions)
		// Compared by what they answer rather than by identity, which is what
		// the caller actually depends on.
		if got.Level(rewards.GoldThresholdV2) != tc.want.Level(rewards.GoldThresholdV2) {
			t.Errorf("TiersForChangeVersions(%v) [%s] resolved to the wrong ladder: "+
				"%d points is %q, want %q", tc.versions, tc.what, rewards.GoldThresholdV2,
				got.Level(rewards.GoldThresholdV2), tc.want.Level(rewards.GoldThresholdV2))
		}
	}
}

// The entry Temporal writes is "<change id>-<version>", and the API matches on
// it literally. Spelled out here so a rename of either half fails a test rather
// than silently resolving every run to v1.
func TestChangeVersionEntryShape(t *testing.T) {
	if want := "tier-thresholds-1"; rewards.ChangeVersionTierThresholds != want {
		t.Errorf("ChangeVersionTierThresholds = %q, want %q -- if the change ID or "+
			"version really moved, every already-recorded run just fell back to v1",
			rewards.ChangeVersionTierThresholds, want)
	}
}
