// Package rewards is the domain layer for the customer rewards Entity Workflow:
// the state it carries, the Update/Query contract every caller speaks, and the
// rules -- tiers, enrollment validity -- expressed as plain functions over that
// state.
//
// It deliberately contains no workflow code; that lives in
// internal/rewards/workflows, which imports this package and nothing more.
package rewards

import (
	"slices"
	"time"
)

// TaskQueue is the single queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowTypeName is how the workflow is registered, and therefore how it is
// addressed in visibility queries. Must match the function name, since that is
// what RegisterWorkflow uses by default -- pinned here so the list endpoint's
// scoping clause and the workflow itself cannot drift apart silently.
const WorkflowTypeName = "CustomerRewardsWorkflow"

// WorkflowIDPrefix makes the workflow ID derivable from the customer ID alone,
// which is what lets every later operation skip a lookup table.
const WorkflowIDPrefix = "customer-"

// WorkflowID returns the deterministic workflow ID for a customer.
func WorkflowID(customerID string) string { return WorkflowIDPrefix + customerID }

// Tier thresholds. Tiers are derived from these, never stored.
const (
	GoldThreshold     = 500
	PlatinumThreshold = 1000
)

// Tier names. Strings rather than a custom type because they cross the wire as
// search attribute values and JSON.
const (
	LevelBasic    = "basic"
	LevelGold     = "gold"
	LevelPlatinum = "platinum"
)

// Tier is one rung of the ladder: a name and the balance that earns it.
//
// Exported, with JSON tags, because the ladder itself travels to the client as
// part of the customer detail response. A single "next threshold" is not enough
// to draw a progress bar -- that needs the rung *below* the customer too -- and
// the alternative to sending the ladder is a second copy of it in the UI, which
// is what this replaced.
type Tier struct {
	Level     string `json:"level"`
	MinPoints int    `json:"minPoints"`
}

// tiers is the ladder, and the only place a threshold is attached to a tier.
// Level and NextTierAt both walk it rather than each carrying their own switch.
//
// LevelBasic is deliberately not a rung. It is the floor -- what you are when
// no rule matches.
//
// MUST stay sorted by MinPoints ascending; everything below relies on it.
// TestTierLadderIsOrdered enforces that rather than trusting the comment.
var tiers = []Tier{
	{Level: LevelGold, MinPoints: GoldThreshold},
	{Level: LevelPlatinum, MinPoints: PlatinumThreshold},
}

// Ladder returns the rungs, ordered by MinPoints ascending.
//
// A copy: Level and NextTierAt read `tiers` trusting that order without
// re-checking it, so a caller that sorted or appended to a shared slice would
// corrupt tier derivation for the whole process.
//
// LevelBasic is absent, because it is the floor rather than a rung (see tiers).
// A client drawing the ladder therefore has to supply the "zero to the first
// rung" span itself -- there is no entry describing it.
func Ladder() []Tier {
	return slices.Clone(tiers)
}

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it leaves no trace in Event History; PointsCap is enforced in the
// *handler*, so breaching it is recorded. That split is the point of the
// exercise, not an accident.
const (
	MaxPointsPerTxn = 1000
	PointsCap       = 100000
)

// EarnsPerRun is how many successful adds a run handles before continuing as
// new. Artificially low so the rollover is easy to watch.
//
// CHANGING THIS BREAKS RUNNING WORKFLOWS. A run whose history records a roll
// after 3 adds will not produce that command at that point on replay under a
// different value, and the replayer refuses a command that does not match the
// recorded event. In dev, terminate existing workflows after changing it.
const EarnsPerRun = 3

// CustomerState is the workflow argument. Everything here has to survive
// continue-as-new, which is why the counters live in state rather than being
// recomputed from history: history is reaped, state is not.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Points only ever increase -- no spending, redemption, expiry or
	// adjustment. That is why there is no separate lifetime total: with a
	// monotonic balance the two are the same number. Adding spending later means
	// reintroducing that field, not repurposing this one.
	Points int `json:"points"`

	// Set on the very first run and carried forward untouched thereafter.
	EnrolledAt time.Time `json:"enrolledAt"`
	// Count of successful adds, ever. Not derivable from Points once history is
	// reaped, and needed to quantify audit-log truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	// Set when the customer leaves, and never cleared: deactivation is one-way,
	// and the run completes once the flag is set. Deliberately not an Active
	// bool: the zero value has to mean active, or continue-as-new payloads
	// written before this field existed would decode every rolled-over customer
	// as inactive on deploy.
	Deactivated bool `json:"deactivated,omitempty"`
}

// Level derives the tier from a balance: the highest rung the balance reaches,
// or basic if it reaches none.
func Level(points int) string {
	level := LevelBasic
	for _, t := range tiers {
		if points >= t.MinPoints {
			level = t.Level
		}
	}
	return level
}

// NextTierAt returns the balance at which the next promotion happens, and false
// if the customer is already at the top tier.
func NextTierAt(points int) (int, bool) {
	// The first rung not yet reached, which is the next one up because the
	// ladder is ordered. Falling out means every rung is behind them.
	for _, t := range tiers {
		if points < t.MinPoints {
			return t.MinPoints, true
		}
	}
	return 0, false
}
