// Package rewards contains the customer rewards Entity Workflow and the state
// it carries. See docs/PLAN.md section 3.
package rewards

import "time"

// TaskQueue is the single queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowIDPrefix makes the workflow ID derivable from the customer ID alone,
// which is what lets every later operation skip a lookup table. See PLAN.md 3.
const WorkflowIDPrefix = "customer-"

// WorkflowID returns the deterministic workflow ID for a customer.
func WorkflowID(customerID string) string { return WorkflowIDPrefix + customerID }

// Tier thresholds. Tiers are derived from these, never stored -- see PLAN.md 3.2
// for the trade-off that choice carries.
const (
	GoldThreshold     = 500
	PlatinumThreshold = 1000
)

// Tier names. Strings rather than a custom type because they cross the wire as
// search attribute values and JSON, and a String() round-trip buys nothing here.
const (
	LevelBasic    = "basic"
	LevelGold     = "gold"
	LevelPlatinum = "platinum"
)

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it leaves no trace in Event History; LifetimePointsCap is enforced in
// the *handler*, so breaching it is recorded. That split is the point of the
// exercise, not an accident -- see PLAN.md 3.4.
const (
	MaxPointsPerTxn   = 1000
	LifetimePointsCap = 100000
)

// CustomerState is the workflow argument. Everything here has to survive
// continue-as-new (Phase 2), which is why the lifetime counters live in state
// rather than being recomputed from history: history is reaped, state is not.
// See PLAN.md 3.1 and 6.3.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
	Email      string `json:"email"`

	Points int `json:"points"`

	// Set on the very first run and carried forward untouched thereafter.
	EnrolledAt         time.Time `json:"enrolledAt"`
	LifetimeEarnEvents int       `json:"lifetimeEarnEvents"`
	LifetimePoints     int       `json:"lifetimePoints"`
	Generation         int       `json:"generation"`

	// Levels already notified about, so an at-least-once Activity delivery does
	// not re-notify after a replay. Unused until Phase 6; carried now so the
	// state shape does not change once workflows are running.
	NotifiedLevels []string `json:"notifiedLevels,omitempty"`
}

// Level derives the tier from a balance.
func Level(points int) string {
	switch {
	case points >= PlatinumThreshold:
		return LevelPlatinum
	case points >= GoldThreshold:
		return LevelGold
	default:
		return LevelBasic
	}
}

// NextTierAt returns the balance at which the next promotion happens, and false
// if the customer is already at the top tier. The UI needs "82 points to gold"
// and deriving it here keeps that rule in one place.
func NextTierAt(points int) (int, bool) {
	switch {
	case points >= PlatinumThreshold:
		return 0, false
	case points >= GoldThreshold:
		return PlatinumThreshold, true
	default:
		return GoldThreshold, true
	}
}
