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
	// 1-based position of the current run in the continue-as-new chain: the
	// enrollment run is 1, and the counter increments exactly once per roll.
	RunNumber int `json:"runNumber"`

	// Set when the customer leaves, and never cleared: deactivation is
	// one-way, and the run completes once the flag is set. Not an Active bool:
	// the zero value must mean active so older continue-as-new payloads keep
	// decoding correctly.
	Deactivated bool `json:"deactivated,omitempty"`
}
