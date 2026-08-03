package rewards

import "time"

// The Update/Query contract the workflow exposes and every caller speaks.

// TaskQueue is the single queue every customer workflow runs on.
const TaskQueue = "rewards"

// WorkflowTypeName is how the workflow is registered and how visibility
// queries address it. Must match the workflow function name.
const WorkflowTypeName = "CustomerRewardsWorkflow"

// Handler names, addressed by string from the API layer and the temporal CLI.
const (
	UpdateAddPoints  = "addPoints"
	UpdateDeactivate = "deactivate"
	QueryGetStatus   = "getStatus"
)

// Error types returned by Update handlers. The API maps these to HTTP status
// codes by type, not by message text.
const (
	ErrTypePointsCapExceeded = "PointsCapExceeded"
	ErrTypeInvalidEnrollment = "InvalidEnrollment"
	ErrTypeDeactivated       = "Deactivated"
)

// AddPointsRequest is the addPoints Update argument.
type AddPointsRequest struct {
	Amount int    `json:"amount"`
	Reason string `json:"reason"`
}

// AddPointsResult is what a successful add returns to the caller.
type AddPointsResult struct {
	Balance int    `json:"balance"`
	Level   string `json:"level"`
}

// DeactivateResult reports whether the Update actually changed anything, so a
// raced duplicate is distinguishable from the real leave.
type DeactivateResult struct {
	Changed bool `json:"changed"`
}

// CustomerStatus is the getStatus Query result.
//
// PrevTierAt and NextTierAt bracket the current segment of the tier climb --
// the rung the customer is standing on (0 for basic) and the one they are
// climbing to (0 at the top). Derived here from the same ladder as Level, so
// the UI can draw progress without holding a copy of the thresholds.
type CustomerStatus struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	PrevTierAt int       `json:"prevTierAt"`
	NextTierAt int       `json:"nextTierAt"`
	EnrolledAt time.Time `json:"enrolledAt"`

	LifetimeEarnEvents int  `json:"lifetimeEarnEvents"`
	RunNumber          int  `json:"runNumber"`
	Active             bool `json:"active"`
}

// StatusOf projects state into the Query result.
func StatusOf(state *CustomerState) CustomerStatus {
	nextAt, _ := NextTierAt(state.Points)
	return CustomerStatus{
		CustomerID:         state.CustomerID,
		Name:               state.Name,
		Points:             state.Points,
		Level:              Level(state.Points),
		PrevTierAt:         PrevTierAt(state.Points),
		NextTierAt:         nextAt,
		EnrolledAt:         state.EnrolledAt,
		LifetimeEarnEvents: state.LifetimeEarnEvents,
		RunNumber:          state.RunNumber,
		Active:             !state.Deactivated,
	}
}
