// Package rewards is the domain layer for the customer rewards Entity Workflow:
// the state it carries, the Update/Query contract, and the tier rules as plain
// functions. Workflow code lives in internal/rewards/workflows.
package rewards

import (
	"slices"
	"time"
)

// TaskQueue is the single queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowTypeName is how the workflow is registered and how visibility
// queries address it. Must match the workflow function name.
const WorkflowTypeName = "CustomerRewardsWorkflow"

// WorkflowIDPrefix makes the workflow ID derivable from the customer ID alone,
// which is what lets every operation skip a lookup table.
const WorkflowIDPrefix = "customer-"

// WorkflowID returns the deterministic workflow ID for a customer.
func WorkflowID(customerID string) string { return WorkflowIDPrefix + customerID }

// Tier thresholds. Tiers are derived from points, never stored.
const (
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
// floor, not a rung.
var tiers = []Tier{
	{Level: LevelGold, MinPoints: GoldThreshold},
	{Level: LevelPlatinum, MinPoints: PlatinumThreshold},
}

// Ladder returns a copy of the rungs, ordered by MinPoints ascending.
func Ladder() []Tier {
	return slices.Clone(tiers)
}

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it writes nothing to Event History; PointsCap is enforced in the
// *handler*, so breaching it is recorded. That split is a point the POC
// demonstrates.
const (
	MaxPointsPerTxn = 1000
	PointsCap       = 5000
)

// EarnsPerRun is how many successful adds a run handles before continuing as
// new. Artificially low so the rollover is easy to watch; production would ask
// workflow.GetInfo(ctx).GetContinueAsNewSuggested() instead.
//
// Changing this breaks running workflows: replay of a history recorded under a
// different value produces different commands. In dev, `make reset` first.
const EarnsPerRun = 3

// CustomerState is the workflow argument. Everything here survives
// continue-as-new; history is reaped after retention, state is not.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Points only ever increase (no spending or expiry), so this is also the
	// lifetime total.
	Points int `json:"points"`

	// Set on the first run, carried forward untouched.
	EnrolledAt time.Time `json:"enrolledAt"`
	// Count of successful adds, ever. Not derivable from history once it is
	// reaped; used to quantify audit-log truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	// Set when the customer leaves, and never cleared: deactivation is
	// one-way, and the run completes once the flag is set. Not an Active bool:
	// the zero value must mean active so older continue-as-new payloads keep
	// decoding correctly.
	Deactivated bool `json:"deactivated,omitempty"`
}

// Level derives the tier from a balance: the highest rung reached, or basic.
func Level(points int) string {
	level := LevelBasic
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
