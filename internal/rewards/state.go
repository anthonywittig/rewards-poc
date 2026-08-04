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
//
// Changing this breaks running workflows: replay of a history recorded under a
// different value produces different commands. In dev, `make destroy && make up`.
const EarnsPerRun = 3

// RecentRequestsCap bounds the dedup ledger carried across continue-as-new:
// two runs' worth of adds, enough to cover a retry that straddles one roll.
const RecentRequestsCap = 2 * EarnsPerRun

// AppliedRequest is one entry in the dedup ledger: an applied add and the
// result it returned, so a duplicate can be answered with the original
// result instead of applied again.
type AppliedRequest struct {
	RequestID string `json:"requestId"`
	Balance   int    `json:"balance"`
	Level     string `json:"level"`
}

// CustomerState is the workflow argument. Everything here survives
// continue-as-new; history is reaped after retention, state is not.
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

	// Execution bookkeeping
	// Count of successful adds, ever. Not derivable from history once it is
	// reaped; used to quantify audit-log truncation.
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	// 1-based position of the current run in the continue-as-new chain: the
	// enrollment run is 1, and the counter increments exactly once per roll.
	RunNumber int `json:"runNumber"`

	// Dedup ledger
	// The most recent applied adds, oldest first, capped at RecentRequestsCap.
	// The server already dedups Update IDs within a run; carrying these across
	// continue-as-new closes the gap where a retry straddles a roll.
	RecentRequests []AppliedRequest `json:"recentRequests,omitempty"`
}

// PriorResult looks up an already-applied add by its Update ID, returning the
// result the original delivery got.
func (s *CustomerState) PriorResult(requestID string) (AddPointsResult, bool) {
	for _, r := range s.RecentRequests {
		if r.RequestID == requestID {
			return AddPointsResult{Balance: r.Balance, Level: r.Level}, true
		}
	}
	return AddPointsResult{}, false
}

// RecordApplied appends an applied add to the dedup ledger, evicting the
// oldest entries beyond RecentRequestsCap.
func (s *CustomerState) RecordApplied(requestID string, res AddPointsResult) {
	s.RecentRequests = append(s.RecentRequests, AppliedRequest{
		RequestID: requestID,
		Balance:   res.Balance,
		Level:     res.Level,
	})
	if n := len(s.RecentRequests) - RecentRequestsCap; n > 0 {
		s.RecentRequests = s.RecentRequests[n:]
	}
}
