package rewards

import "slices"

// Tier thresholds. Tiers are derived from points, never stored.
const (
	BasicThreshold    = 0
	GoldThreshold     = 500
	PlatinumThreshold = 1000
)

// Tier names, as they appear in search attributes and JSON.
const (
	LevelBasic    = "basic"
	LevelGold     = "gold"
	LevelPlatinum = "platinum"
)

// Tier is one rung of the ladder. It travels to the client in the detail
// response so the UI can draw a progress bar without its own copy of the
// thresholds.
type Tier struct {
	Level     string `json:"level"`
	MinPoints int    `json:"minPoints"`
}

// tiers is the ladder, ordered by MinPoints ascending. LevelBasic is the
// bottom rung: its MinPoints of 0 means every balance stands on some rung.
var tiers = []Tier{
	{Level: LevelBasic, MinPoints: BasicThreshold},
	{Level: LevelGold, MinPoints: GoldThreshold},
	{Level: LevelPlatinum, MinPoints: PlatinumThreshold},
}

// Ladder returns a copy of the rungs, ordered by MinPoints ascending.
func Ladder() []Tier {
	return slices.Clone(tiers)
}

// Level derives the tier from a balance: the highest rung reached. The bottom
// rung starts at 0, so every balance reaches at least one.
func Level(points int) string {
	level := tiers[0].Level
	for _, t := range tiers {
		if points >= t.MinPoints {
			level = t.Level
		}
	}
	return level
}

// NextTierAt returns the balance at which the next promotion happens, and
// false if the customer is already at the top tier.
func NextTierAt(points int) (int, bool) {
	for _, t := range tiers {
		if points < t.MinPoints {
			return t.MinPoints, true
		}
	}
	return 0, false
}

// PrevTierAt returns the balance at which the customer's current tier began:
// the MinPoints of the highest rung reached. With NextTierAt it brackets the
// segment of the climb a progress bar draws.
func PrevTierAt(points int) int {
	at := tiers[0].MinPoints
	for _, t := range tiers {
		if points >= t.MinPoints {
			at = t.MinPoints
		}
	}
	return at
}
