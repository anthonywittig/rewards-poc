// Package rewards is the domain layer for the customer rewards Entity Workflow:
// the state it carries, the Update/Query contract every caller speaks, and the
// rules -- tiers, enrollment validity, which promotion is owed -- expressed as
// plain functions over that state. See docs/FINDINGS.md#workflow-design.
//
// It deliberately contains no workflow or Activity code:
//
//	internal/rewards            this package: types and pure logic
//	internal/rewards/workflows  CustomerRewardsWorkflow and its handlers
//	internal/rewards/activities the Activities struct, and the only code here
//	                            allowed to touch the outside world
//
// Both sub-packages import this one; neither imports the other. That last part
// is load-bearing: the Go SDK has no workflow sandbox, so nothing at runtime
// stops workflow code from calling a database handle directly and silently
// breaking determinism. Workflow code names the Activity by the
// ActivityNotifyCustomer string constant instead.
package rewards

import (
	"fmt"
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
// FINDINGS.md#tiers-are-derived-never-stored.
//
// There are two sets because lowering the bar is a *command-changing* edit: the
// balance at which a promotion Activity is scheduled moves, and so do the values
// every search attribute upsert writes. Editing the constants in place would
// wedge every run already in flight, so the ladder is versioned instead --
// FINDINGS.md#versioning-the-tier-thresholds.
const (
	// v1, the original ladder. Runs whose history predates the version marker
	// keep it for the rest of their lives.
	GoldThreshold     = 500
	PlatinumThreshold = 1000

	// TierThresholdDrop is how much cheaper v2 makes every rung. One constant
	// rather than two hand-written numbers, so "50 less" cannot drift into "50
	// less for gold, 40 less for platinum".
	TierThresholdDrop = 50

	// v2, the ladder a run started today uses.
	GoldThresholdV2     = GoldThreshold - TierThresholdDrop
	PlatinumThresholdV2 = PlatinumThreshold - TierThresholdDrop
)

// Versioning marker for the tier ladder, and the only thing in this package that
// exists for workflow.GetVersion. It lives here rather than in the workflows
// package because two callers need it: the workflow passes it to GetVersion, and
// the API reads it back out of the built-in TemporalChangeVersion search
// attribute to tell which ladder a run it cannot Query was using.
//
// The name is recorded in Event History and can never be reused or renamed.
// FINDINGS.md#versioning-is-the-real-risk.
const (
	ChangeTierThresholds  = "tier-thresholds"
	VersionTierThresholds = 1
)

// ChangeVersionTierThresholds is the TemporalChangeVersion entry Temporal writes
// for a run that resolved the marker above to VersionTierThresholds. Its shape
// is "<change id>-<version>", set by the SDK rather than by us.
// FINDINGS.md#getversion-writes-two-events.
var ChangeVersionTierThresholds = fmt.Sprintf("%s-%d", ChangeTierThresholds, VersionTierThresholds)

// Tier names. Strings rather than a custom type because they cross the wire as
// search attribute values and JSON.
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

// TierLadder is an ordered ladder of rungs, and the only place a threshold is
// attached to a tier. Level, NextTierAt, TierFloor and PromotionFor are all
// methods on it rather than each carrying their own switch.
//
// LevelBasic is deliberately not a rung. It is the floor -- what you are when no
// rule matches -- and giving it a zero-point rule would make "promoted to basic"
// something the notifier had to special-case back out.
//
// MUST stay sorted by MinPoints ascending; everything below relies on it.
// TestTierLadderIsOrdered enforces that for every ladder in ladders rather than
// trusting the comment.
type TierLadder []tier

// The ladders. A run picks one at startup from its GetVersion result and uses
// that one for its whole life; nothing recomputes a tier under a different
// ladder than the one its history was written with.
var (
	TiersV1 = TierLadder{
		{Level: LevelGold, MinPoints: GoldThreshold},
		{Level: LevelPlatinum, MinPoints: PlatinumThreshold},
	}
	TiersV2 = TierLadder{
		{Level: LevelGold, MinPoints: GoldThresholdV2},
		{Level: LevelPlatinum, MinPoints: PlatinumThresholdV2},
	}
)

// ladders is every ladder that exists, for the tests that assert the invariants
// each one has to satisfy. Not for dispatch: use TiersForChangeVersions.
//
// There is deliberately no exported "the current ladder" alongside it. Every
// caller either belongs to a run, and takes that run's ladder, or is resolving
// one from a change version -- and a package-level shortcut is exactly how a
// tier gets derived under thresholds the customer's run never used.
var ladders = map[string]TierLadder{"v1": TiersV1, "v2": TiersV2}

// TiersForChangeVersions resolves the ladder from a run's TemporalChangeVersion
// search attribute, which is how a caller outside the workflow -- the API
// projecting a closed execution it cannot Query -- gets the same answer the run
// itself would have given.
//
// Absent entry means absent marker means DefaultVersion, which is v1. That is
// the same mapping the workflow makes, and it has to stay that way: a closed
// pre-marker customer whose detail page suddenly reads "gold at 450" would be
// showing them a promotion their run never made.
func TiersForChangeVersions(changeVersions []string) TierLadder {
	if slices.Contains(changeVersions, ChangeVersionTierThresholds) {
		return TiersV2
	}
	return TiersV1
}

// Validation limits. MaxPointsPerTxn is enforced in the Update *validator*, so
// breaching it leaves no trace in Event History; PointsCap is enforced in the
// *handler*, so breaching it is recorded. That split is the point of the
// exercise, not an accident -- see FINDINGS.md#the-validatorhandler-split.
const (
	MaxPointsPerTxn = 1000
	PointsCap       = 100000
)

// EarnsPerRun is how many successful adds a run handles before continuing as
// new. Artificially low so the rollover is easy to watch.
//
// A floor rather than an exact count: the main loop delivers any pending
// promotion before it rolls, and the handler keeps accepting adds for the
// duration of that Activity -- measured at 4 adds when a tier crossing lands in
// the rolling run, 3 when none does.
// FINDINGS.md#earnsperrun-is-a-floor-not-an-exact-count.
//
// CHANGING THIS BREAKS RUNNING WORKFLOWS. A run whose history records a roll
// after 3 adds will not produce that command at that point on replay under a
// different value, and the replayer refuses a command that does not match the
// recorded event. In dev, terminate existing workflows after changing it.
// FINDINGS.md#versioning-is-the-real-risk.
const EarnsPerRun = 3

// CustomerState is the workflow argument. Everything here has to survive
// continue-as-new, which is why the counters live in state rather than being
// recomputed from history: history is reaped, state is not.
// FINDINGS.md#the-workflow-is-the-integrity-boundary.
type CustomerState struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
	Email      string `json:"email"`

	// Points only ever increase -- no spending, redemption, expiry or
	// adjustment. That is why there is no separate lifetime total: with a
	// monotonic balance the two are the same number. Adding spending later means
	// reintroducing that field, not repurposing this one.
	Points int `json:"points"`

	// Set on the very first run and carried forward untouched thereafter.
	EnrolledAt time.Time `json:"enrolledAt"`
	// Count of successful adds, ever. Not derivable from Points once history is
	// reaped, and FINDINGS.md#truncation-detection needs it to quantify audit-log
	// truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	// Levels already notified about, so an at-least-once Activity delivery does
	// not re-notify after a replay.
	NotifiedLevels []string `json:"notifiedLevels,omitempty"`

	// Set when the customer leaves; cleared on re-enrollment. Deliberately not
	// an Active bool: the zero value has to mean active, or continue-as-new
	// payloads written before this field existed would decode every rolled-over
	// customer as inactive on deploy.
	Deactivated bool `json:"deactivated,omitempty"`
}

// Level derives the tier from a balance: the highest rung the balance reaches,
// or basic if it reaches none.
func (l TierLadder) Level(points int) string {
	level := LevelBasic
	for _, t := range l {
		if points >= t.MinPoints {
			level = t.Level
		}
	}
	return level
}

// NextTierAt returns the balance at which the next promotion happens, and false
// if the customer is already at the top tier.
func (l TierLadder) NextTierAt(points int) (int, bool) {
	// The first rung not yet reached, which is the next one up because the
	// ladder is ordered. Falling out means every rung is behind them.
	for _, t := range l {
		if points < t.MinPoints {
			return t.MinPoints, true
		}
	}
	return 0, false
}

// TierFloor returns the balance that earned the customer their current tier, or
// 0 for basic.
//
// It exists for the progress bar, which needs both ends of the current rung to
// draw. The UI used to hardcode "the rung below 1000 is 500"; once the ladder is
// versioned that is no longer knowable from the next threshold alone, so the
// span comes from whichever ladder the run is actually on.
func (l TierLadder) TierFloor(points int) int {
	floor := 0
	for _, t := range l {
		if points >= t.MinPoints {
			floor = t.MinPoints
		}
	}
	return floor
}
