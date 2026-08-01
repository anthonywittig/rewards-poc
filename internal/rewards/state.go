// Package rewards is the domain layer for the customer rewards Entity Workflow:
// the state it carries, the Update/Query contract every caller speaks, and the
// rules -- tiers, enrollment validity, which promotion is owed -- expressed as
// plain functions over that state. See docs/PLAN.md section 3.
//
// It deliberately contains no workflow or Activity code. Those live in the two
// sub-packages, and the split is structural rather than cosmetic:
//
//	internal/rewards            this package: types and pure logic
//	internal/rewards/workflows  CustomerRewardsWorkflow and its handlers
//	internal/rewards/activities the Activities struct, and the only code here
//	                            allowed to touch the outside world
//
// Both sub-packages import this one; neither imports the other. That last part
// is the point. The Go SDK has no workflow sandbox, so nothing at runtime stops
// workflow code from calling a database handle directly and silently breaking
// determinism -- a package boundary is the only structural guard available, and
// it only works while workflows cannot reach activities. Workflow code names the
// Activity by the ActivityNotifyCustomer string constant below instead.
package rewards

import "time"

// TaskQueue is the single queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowTypeName is how the workflow is registered, and therefore how it is
// addressed in visibility queries. Must match the function name, since that is
// what RegisterWorkflow uses by default -- pinned here so the list endpoint's
// scoping clause and the workflow itself cannot drift apart silently.
const WorkflowTypeName = "CustomerRewardsWorkflow"

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

// tier is one rung of the ladder: a name and the balance that earns it.
type tier struct {
	Level     string
	MinPoints int
}

// tiers is the ladder, and the only place a threshold is attached to a tier.
// Read it as the rule list it is:
//
//	when points >= GoldThreshold:     gold
//	when points >= PlatinumThreshold: platinum
//
// Level, NextTierAt and promotionFor all walk this rather than each carrying
// their own switch. Three switches over the same two constants is three places
// to edit when a tier is added and two places to forget -- and the failure is
// quiet: a missing case in NextTierAt is a wrong progress bar, and a missing
// case in promotionFor is a promotion nobody is told about.
//
// LevelBasic is deliberately not a rung. It is the floor -- what you are when no
// rule matches -- and giving it a zero-point rule would make "promoted to basic"
// something the notifier had to special-case back out.
//
// MUST stay sorted by MinPoints ascending; everything below relies on it.
// TestTierLadderIsOrdered enforces that rather than trusting the comment.
var tiers = []tier{
	{Level: LevelGold, MinPoints: GoldThreshold},
	{Level: LevelPlatinum, MinPoints: PlatinumThreshold},
}

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it leaves no trace in Event History; PointsCap is enforced in the
// *handler*, so breaching it is recorded. That split is the point of the
// exercise, not an accident -- see PLAN.md 3.4.
const (
	MaxPointsPerTxn = 1000
	PointsCap       = 100000
)

// EarnsPerRun is how many successful adds a run handles before continuing as
// new. Artificially low so the rollover is easy to watch -- see the note at the
// continue-as-new itself for what production should do instead.
//
// It is also a floor rather than an exact count. The main loop delivers any
// pending promotion before it rolls, and the handler keeps accepting adds for
// the duration of that Activity -- measured at 4 adds in the rolling run when a
// tier crossing lands in it, against exactly 3 when none does. The number
// survived the rewrite from a drain goroutine to main-loop delivery unchanged,
// which is what you would expect: the cause is the Activity's round trip rather
// than the structure around it. PLAN.md 12.32.
//
// CHANGING THIS BREAKS RUNNING WORKFLOWS. A run whose history already records a
// roll after 3 adds will, on replay under a different value, not produce that
// command at that point, and a command that does not match the recorded event
// is exactly what the replayer refuses. Entity workflows outlive deploys, so
// this is not theoretical; PLAN.md 12.10. In dev, terminate existing workflows
// after changing it.
const EarnsPerRun = 3

// CustomerState is the workflow argument. Everything here has to survive
// continue-as-new (Phase 2), which is why the counters live in state rather than
// being recomputed from history: history is reaped, state is not.
// See PLAN.md 3.1 and 6.3.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
	Email      string `json:"email"`

	// Points only ever increase. There is no spending, redemption, expiry or
	// adjustment in this POC and none is planned -- see PLAN.md 3.1.
	//
	// That is why there is no separate lifetime total: with a monotonic balance,
	// "points now" and "points ever earned" are the same number, and carrying
	// both would only create an invariant to violate. Adding spending later
	// means reintroducing that field, not repurposing this one.
	Points int `json:"points"`

	// Set on the very first run and carried forward untouched thereafter.
	EnrolledAt time.Time `json:"enrolledAt"`
	// Count of successful adds, ever. Not derivable from Points once history is
	// reaped, and PLAN.md 6.3 needs it to quantify audit-log truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	// Levels already notified about, so an at-least-once Activity delivery does
	// not re-notify after a replay.
	NotifiedLevels []string `json:"notifiedLevels,omitempty"`

	// Set when the customer leaves; cleared on re-enrollment. Zero value
	// (false) means active, which is also what continue-as-new payloads from
	// before this field existed correctly mean -- an inverted Active bool
	// would have marked every rolled-over customer inactive on deploy.
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
// if the customer is already at the top tier. The UI needs "82 points to gold"
// and deriving it here keeps that rule in one place.
func NextTierAt(points int) (int, bool) {
	// The first rung not yet reached, which is the next one up because the ladder
	// is ordered. Falling out of the loop means every rung is behind them.
	for _, t := range tiers {
		if points < t.MinPoints {
			return t.MinPoints, true
		}
	}
	return 0, false
}
