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

// AddsPerRun is how many handled addPoints Updates a run takes before
// continuing as new. Handled, not successful: a cap rejection runs the handler
// and writes Update events to history just like an add that lands, so both
// count — otherwise a customer parked at the cap would grow one run's history
// forever without ever rolling it. Artificially low so the rollover is easy to
// watch; the workflow also rolls whenever the server suggests it
// (GetContinueAsNewSuggested), the signal production would rely on alone.
//
// Changing this breaks running workflows: replay of a history recorded under a
// different value produces different commands. In dev, `make destroy && make up`.
const AddsPerRun = 3

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
}
