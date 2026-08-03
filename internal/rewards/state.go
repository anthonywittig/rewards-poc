// Package rewards is the domain layer for the customer rewards Entity
// Workflow: the state it carries, the Update/Query contract every caller
// speaks, and the tier rules as plain functions over that state.
//
// It contains no workflow code; that lives in internal/rewards/workflows,
// which imports this package and nothing more.
package rewards

import "time"

// TaskQueue is the queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowIDPrefix makes the workflow ID derivable from the customer ID
// alone, which is what lets every operation skip a lookup table.
const WorkflowIDPrefix = "customer-"

// WorkflowID returns the deterministic workflow ID for a customer.
func WorkflowID(customerID string) string { return WorkflowIDPrefix + customerID }

// Tier thresholds. Tiers are derived from the balance, never stored.
const (
	GoldThreshold     = 500
	PlatinumThreshold = 1000
)

// Tier names. Strings because they cross the wire as search attribute values
// and JSON.
const (
	LevelBasic    = "basic"
	LevelGold     = "gold"
	LevelPlatinum = "platinum"
)

// tier is one rung of the ladder. LevelBasic is the floor -- what you are when
// no rung is reached -- so it is deliberately not an entry.
type tier struct {
	level     string
	minPoints int
}

// tiers MUST stay sorted by minPoints ascending; Level and NextTierAt walk it
// trusting that order. TestTierLadderIsOrdered enforces it.
var tiers = []tier{
	{level: LevelGold, minPoints: GoldThreshold},
	{level: LevelPlatinum, minPoints: PlatinumThreshold},
}

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it leaves no trace in Event History; PointsCap is enforced in the
// *handler*, so breaching it is permanently recorded. That split is the demo,
// not an accident.
const (
	MaxPointsPerTxn = 1000
	PointsCap       = 100000
)

// EarnsPerRun is how many successful adds a run handles before continuing as
// new -- artificially low so the rollover is easy to watch. Production should
// ask workflow.GetInfo(ctx).GetContinueAsNewSuggested() instead.
//
// CHANGING THIS BREAKS RUNNING WORKFLOWS: the roll point is baked into
// recorded history, and replay refuses commands that no longer match it. In
// dev, `make reset` and start over.
const EarnsPerRun = 3

// CustomerState is the workflow argument. Everything a customer must not lose
// across continue-as-new lives here.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Points only ever increase -- no spending, redemption, expiry or
	// adjustment -- so the balance is also the lifetime total.
	Points int `json:"points"`

	// Set on the very first run, carried forward untouched thereafter.
	EnrolledAt time.Time `json:"enrolledAt"`

	// Incremented on every continue-as-new.
	Generation int `json:"generation"`

	// Set when the customer leaves; cleared on re-enrollment. Not an Active
	// bool: the zero value has to mean "active", or payloads recorded before
	// this field existed would decode every customer as inactive.
	Deactivated bool `json:"deactivated,omitempty"`
}

// Level derives the tier from a balance: the highest rung the balance
// reaches, or basic if it reaches none.
func Level(points int) string {
	level := LevelBasic
	for _, t := range tiers {
		if points >= t.minPoints {
			level = t.level
		}
	}
	return level
}

// NextTierAt returns the balance at which the next promotion happens, and
// false if the customer is already at the top tier.
func NextTierAt(points int) (int, bool) {
	for _, t := range tiers {
		if points < t.minPoints {
			return t.minPoints, true
		}
	}
	return 0, false
}
