package rewards

import "testing"

// The ladder's ordering is load-bearing, and this is in-package because tiers is
// unexported. Level takes the last rung it reaches and NextTierAt the first it
// has not -- both go wrong if the entries are not sorted by MinPoints ascending,
// and both go wrong *quietly*: no panic, just a customer told they are gold when
// they are platinum.
func TestTierLadderIsOrdered(t *testing.T) {
	if len(tiers) == 0 {
		t.Fatal("the ladder is empty; Level would always answer basic")
	}
	for i, tier := range tiers {
		if tier.Level == LevelBasic {
			t.Errorf("tiers[%d] is basic, which is the floor rather than a rung", i)
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
