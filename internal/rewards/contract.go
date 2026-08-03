package rewards

import "time"

// The Update/Query contract the workflow exposes and every caller speaks.

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
type CustomerStatus struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"` // 0 when already at the top tier
	EnrolledAt time.Time `json:"enrolledAt"`

	// The ladder Level and NextTierAt were derived from, so the UI never holds
	// its own copy of the thresholds.
	Tiers []Tier `json:"tiers"`

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
		NextTierAt:         nextAt,
		Tiers:              Ladder(),
		EnrolledAt:         state.EnrolledAt,
		LifetimeEarnEvents: state.LifetimeEarnEvents,
		RunNumber:          state.RunNumber,
		Active:             !state.Deactivated,
	}
}
