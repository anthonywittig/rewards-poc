package rewards

import "time"

// The Update/Query contract. Every caller -- the `temporal` CLI included --
// addresses handlers by these names, and a typo there is a runtime error.
const (
	UpdateAddPoints  = "addPoints"
	UpdateDeactivate = "deactivate"
	UpdateReactivate = "reactivate"
	QueryGetStatus   = "getStatus"
)

// Error types returned by Update handlers, so callers can match on a type
// rather than on an error message.
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

// DeactivateResult and ReactivateResult report whether anything changed,
// which is how a real transition is told apart from an idempotent repeat.
type DeactivateResult struct {
	Changed bool `json:"changed"`
}

// ReactivateResult mirrors DeactivateResult.
type ReactivateResult struct {
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
	Generation int       `json:"generation"`
	Active     bool      `json:"active"`
}

// StatusOf projects state into the Query result, deriving the tier fields.
func StatusOf(state *CustomerState) CustomerStatus {
	nextAt, _ := NextTierAt(state.Points)
	return CustomerStatus{
		CustomerID: state.CustomerID,
		Name:       state.Name,
		Points:     state.Points,
		Level:      Level(state.Points),
		NextTierAt: nextAt,
		EnrolledAt: state.EnrolledAt,
		Generation: state.Generation,
		Active:     !state.Deactivated,
	}
}
