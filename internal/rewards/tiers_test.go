package rewards

import "testing"

// Every ladder has to satisfy these, not just the current one: a run pinned to
// TiersV1 by its GetVersion marker walks that ladder for the rest of its life,
// so a broken older ladder is a live bug rather than history.
//
// In-package because tier is unexported.

// The ladder's ordering is load-bearing. Level takes the last rung it reaches,
// NextTierAt the first it has not, TierFloor the last it has, and PromotionFor
// walks it backwards -- all four go wrong if the entries are not sorted by
// MinPoints ascending, and all four go wrong *quietly*: no panic, just a
// customer told they are gold when they are platinum.
func TestTierLadderIsOrdered(t *testing.T) {
	for name, ladder := range ladders {
		if len(ladder) == 0 {
			t.Errorf("%s is empty; Level would always answer basic", name)
		}
		for i, tier := range ladder {
			if tier.Level == LevelBasic {
				t.Errorf("%s[%d] is basic, which is the floor rather than a rung: "+
					"a rule for it would make PromotionFor offer 'promoted to basic'", name, i)
			}
			if tier.MinPoints <= 0 {
				t.Errorf("%s[%d] (%s) has MinPoints %d; a rung at or below zero is "+
					"reached by every customer at enrollment", name, i, tier.Level, tier.MinPoints)
			}
			if i > 0 && tier.MinPoints <= ladder[i-1].MinPoints {
				t.Errorf("%s[%d] (%s at %d) does not come after %s[%d] (%s at %d); "+
					"the ladder must be sorted by MinPoints ascending",
					name, i, tier.Level, tier.MinPoints,
					name, i-1, ladder[i-1].Level, ladder[i-1].MinPoints)
			}
		}
	}
}

// The ladder is what Level answers with, so a duplicate name would make one rung
// unreachable by NotifiedLevels: announcing the lower one would suppress the
// higher.
func TestTierLevelsAreDistinct(t *testing.T) {
	for name, ladder := range ladders {
		seen := make(map[string]int, len(ladder))
		for i, tier := range ladder {
			if first, dup := seen[tier.Level]; dup {
				t.Errorf("%s[%d] repeats the level %q from %s[%d]; NotifiedLevels "+
					"keys on the name, so the second would be suppressed by the first",
					name, i, tier.Level, name, first)
			}
			seen[tier.Level] = i
		}
	}
}

// v2 is v1 with every rung lowered by the same amount. Asserted against the
// ladders rather than against the constants, so adding a tier to one ladder and
// forgetting the other is a failure rather than a customer who can never be
// promoted past gold.
func TestV2IsV1LoweredByTheDrop(t *testing.T) {
	if len(TiersV2) != len(TiersV1) {
		t.Fatalf("TiersV2 has %d rungs, TiersV1 has %d; the ladders must stay "+
			"rung-for-rung or a customer's tier changes shape at continue-as-new",
			len(TiersV2), len(TiersV1))
	}
	for i := range TiersV1 {
		if TiersV2[i].Level != TiersV1[i].Level {
			t.Errorf("rung %d is %q in v1 and %q in v2", i, TiersV1[i].Level, TiersV2[i].Level)
			continue
		}
		if got, want := TiersV2[i].MinPoints, TiersV1[i].MinPoints-TierThresholdDrop; got != want {
			t.Errorf("%s is %d in v2, want %d (%d less than v1's %d)",
				TiersV2[i].Level, got, want, TierThresholdDrop, TiersV1[i].MinPoints)
		}
	}
}
