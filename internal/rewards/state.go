// Package rewards is the domain layer for the customer rewards Entity Workflow:
// the state it carries, the Update/Query contract, and the tier rules as plain
// functions. Workflow code lives in internal/rewards/workflows.
package rewards

import "time"

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
const EarnsPerRun = 3

// RecentRequestIDCap bounds RecentRequestIDs. Only successful adds record an
// ID, at most EarnsPerRun per run, so this remembers several rolls back —
// far longer than any client retry loop lives — while keeping the carried
// state a fixed size no matter the traffic.
const RecentRequestIDCap = 20

// CustomerState is the workflow argument. Everything here is carried forward
// untouched by continue-as-new.
type CustomerState struct {
	// Identity
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Balance — only ever increases (no spending or expiry).
	Points int `json:"points"`

	// Membership lifecycle
	// Set on the first run, carried forward untouched.
	EnrolledAt time.Time `json:"enrolledAt"`
	// True while the customer is enrolled. Cleared once by deactivate; the
	// run then completes instead of continuing as new. Deactivation is
	// one-way — Active is never set back to true.
	Active bool `json:"active"`

	// Idempotency
	// Request IDs of recent successful adds, oldest first, at most
	// RecentRequestIDCap entries. The Temporal server dedups Update IDs
	// within a run; carrying these across continue-as-new lets the next
	// run's validator recognise a retry that straddled the roll.
	RecentRequestIDs []string `json:"recentRequestIds,omitempty"`

	// Execution bookkeeping
	// Count of successful adds, ever. Not derivable from history once it is
	// reaped; used to quantify audit-log truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	// 1-based position of the current run in the continue-as-new chain: the
	// enrollment run is 1, and the counter increments exactly once per roll.
	RunNumber int `json:"runNumber"`
}

// SeenRequestID reports whether id was recorded by a recent successful add.
// The empty ID is never seen: a caller that sends no idempotency key has
// opted out of dedup.
func (s *CustomerState) SeenRequestID(id string) bool {
	if id == "" {
		return false
	}
	for _, seen := range s.RecentRequestIDs {
		if seen == id {
			return true
		}
	}
	return false
}

// RecordRequestID remembers id as applied, evicting the oldest entries once
// the cap is reached. Recording the empty ID is a no-op.
func (s *CustomerState) RecordRequestID(id string) {
	if id == "" {
		return
	}
	s.RecentRequestIDs = append(s.RecentRequestIDs, id)
	if over := len(s.RecentRequestIDs) - RecentRequestIDCap; over > 0 {
		s.RecentRequestIDs = append([]string(nil), s.RecentRequestIDs[over:]...)
	}
}
