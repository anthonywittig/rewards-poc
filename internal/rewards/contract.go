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

// CustomerStatus is the getStatus Query result.
type CustomerStatus struct {
	// Identity
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Balance and tier
	Points     int    `json:"points"`
	Level      string `json:"level"`
	PrevTierAt int    `json:"prevTierAt"`
	NextTierAt int    `json:"nextTierAt"`

	// Membership lifecycle
	EnrolledAt time.Time `json:"enrolledAt"`
	Active     bool      `json:"active"`

	// Execution bookkeeping
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	RunNumber          int `json:"runNumber"`
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
		Active:             !state.Deactivated,
		LifetimeEarnEvents: state.LifetimeEarnEvents,
		RunNumber:          state.RunNumber,
	}
}
