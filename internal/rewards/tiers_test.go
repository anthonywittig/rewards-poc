package rewards

import "testing"

// The ladder's ordering is load-bearing, and in-package because tiers is
// unexported. Level takes the last rung it reaches, NextTierAt takes the first
// it has not, and promotionFor walks it backwards -- all three are wrong if the
// entries are not sorted by MinPoints ascending, and all three are wrong
// *quietly*: no panic, just a customer told they are gold when they are
// platinum. A new tier is one line in the table, so this is the guard against
// that line going in the wrong place.
func TestTierLadderIsOrdered(t *testing.T) {
	if len(tiers) == 0 {
		t.Fatal("the ladder is empty; Level would always answer basic")
	}
	for i, tier := range tiers {
		if tier.Level == LevelBasic {
			t.Errorf("tiers[%d] is basic, which is the floor rather than a rung: "+
				"a rule for it would make promotionFor offer 'promoted to basic'", i)
		}
		if tier.MinPoints <= 0 {
			t.Errorf("tiers[%d] (%s) has MinPoints %d; a rung at or below zero is "+
				"reached by every customer at enrollment", i, tier.Level, tier.MinPoints)
		}
		if i > 0 && tier.MinPoints <= tiers[i-1].MinPoints {
			t.Errorf("tiers[%d] (%s at %d) does not come after tiers[%d] (%s at %d); "+
				"the ladder must be sorted by MinPoints ascending",
				i, tier.Level, tier.MinPoints,
				i-1, tiers[i-1].Level, tiers[i-1].MinPoints)
		}
	}
}

// The ladder is what Level answers with, so a duplicate name would make one rung
// unreachable by NotifiedLevels: announcing the lower one would suppress the
// higher.
func TestTierLevelsAreDistinct(t *testing.T) {
	seen := make(map[string]int, len(tiers))
	for i, tier := range tiers {
		if first, dup := seen[tier.Level]; dup {
			t.Errorf("tiers[%d] repeats the level %q from tiers[%d]; NotifiedLevels "+
				"keys on the name, so the second would be suppressed by the first",
				i, tier.Level, first)
		}
		seen[tier.Level] = i
	}
}
