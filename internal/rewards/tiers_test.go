package rewards

import (
	"slices"
	"testing"
)

// The ladder's ordering is load-bearing, and this is in-package because tiers is
// unexported. Level takes the last rung it reaches, NextTierAt the first it has
// not, and PromotionFor walks it backwards -- all three go wrong if the entries
// are not sorted by MinPoints ascending, and all three go wrong *quietly*: no
// panic, just a customer told they are gold when they are platinum.
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

// Ladder hands the rungs to the API, and from there to a client that is free to
// sort or append. Level, NextTierAt and PromotionFor all read the package's own
// slice trusting the ordering TestTierLadderIsOrdered pins, and never re-check
// it -- so if Ladder aliased that slice, one client mutating what it was given
// would silently mis-derive every customer's tier process-wide.
func TestLadderIsACopy(t *testing.T) {
	got := Ladder()
	if len(got) != len(tiers) {
		t.Fatalf("Ladder() has %d rungs, want %d", len(got), len(tiers))
	}

	// Reversing the order is the mutation that does damage without being
	// obviously invalid: every rung is still present and still correct.
	slices.Reverse(got)

	if tiers[0].Level != LevelGold {
		t.Errorf("mutating Ladder()'s result reordered the package ladder: "+
			"tiers[0] is now %q", tiers[0].Level)
	}
	// Level takes the *last* rung the balance reaches, so a reversed ladder
	// demotes anyone at the top: platinum is checked first and gold overwrites
	// it. The wrong answer is a plausible one, which is the point.
	if lvl := Level(PlatinumThreshold); lvl != LevelPlatinum {
		t.Errorf("Level(%d) = %q after a caller mutated Ladder()'s result, want %q",
			PlatinumThreshold, lvl, LevelPlatinum)
	}
}

// Basic is the floor, not a rung, so it is absent from the ladder by design --
// which means a client cannot render the first segment from the rungs alone and
// has to supply the zero-to-first-rung span itself. Pinned because the UI's
// progress bar depends on it: were basic to appear at MinPoints 0, the bar's
// floor lookup would still work, but PromotionFor would start congratulating
// people for being basic.
func TestLadderOmitsBasic(t *testing.T) {
	for _, rung := range Ladder() {
		if rung.Level == LevelBasic {
			t.Errorf("Ladder() includes %q; basic is the floor, not a rung", LevelBasic)
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
