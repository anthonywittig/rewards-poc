package rewards

import "testing"

// The ladder's ordering is load-bearing: Level takes the last rung it reaches
// and NextTierAt the first it has not, and both go wrong *quietly* if the
// entries are not sorted by minPoints ascending.
func TestTierLadderIsOrdered(t *testing.T) {
	if len(tiers) == 0 {
		t.Fatal("the ladder is empty; Level would always answer basic")
	}
	for i, tr := range tiers {
		if tr.level == LevelBasic {
			t.Errorf("tiers[%d] is basic, which is the floor rather than a rung", i)
		}
		if tr.minPoints <= 0 {
			t.Errorf("tiers[%d] (%s) has minPoints %d; a rung at or below zero is "+
				"reached by every customer at enrollment", i, tr.level, tr.minPoints)
		}
		if i > 0 && tr.minPoints <= tiers[i-1].minPoints {
			t.Errorf("tiers[%d] (%s at %d) does not come after tiers[%d] (%s at %d); "+
				"the ladder must be sorted by minPoints ascending",
				i, tr.level, tr.minPoints,
				i-1, tiers[i-1].level, tiers[i-1].minPoints)
		}
	}
}
