package rewards

import "time"

// The Update/Query contract, and the derived view the Query answers with. It
// lives in the domain package so internal/httpapi never has to import workflow
// code to know the shape of a request.

// Handler names. Exported because the API layer and the `temporal` CLI both
// address handlers by string, and a typo there is a runtime error.
const (
	UpdateAddPoints  = "addPoints"
	UpdateDeactivate = "deactivate"
	UpdateReactivate = "reactivate"
	QueryGetStatus   = "getStatus"
)

// Error types returned by Update handlers. The API layer maps these to HTTP
// status codes; naming them here keeps that mapping from being a string match
// on an error message.
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
	EventID string `json:"eventId"`
}

// DeactivateResult is what deactivate returns so the audit crawl can tell a
// real leave from an idempotent no-op (repeat DELETE).
type DeactivateResult struct {
	Changed bool `json:"changed"`
}

// ReactivateRequest is the reactivate Update argument (re-enrollment).
type ReactivateRequest struct {
	Name string `json:"name"`
}

// ReactivateResult mirrors DeactivateResult: Changed distinguishes a real
// re-enrollment from a no-op against a customer who was already active. The API
// tells a restore (200) from a duplicate enrollment (409) by it.
type ReactivateResult struct {
	Changed bool           `json:"changed"`
	Status  CustomerStatus `json:"status"`
}

// CustomerStatus is the getStatus Query result.
type CustomerStatus struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"` // 0 when already at the top tier
	EnrolledAt time.Time `json:"enrolledAt"`

	// The ladder Level and NextTierAt were read from. Answered by the workflow
	// rather than assembled by the API so all three agree: the api and the
	// worker are separate binaries and separate deploys, so an API that
	// attached its own ladder could pair a NextTierAt from one build with rungs
	// from another during a rollout, and draw a target that is not on the
	// ladder beside it.
	Tiers []Tier `json:"tiers"`

	LifetimeEarnEvents int  `json:"lifetimeEarnEvents"`
	Generation         int  `json:"generation"`
	Active             bool `json:"active"`
}

// StatusOf projects state into the Query result, deriving the tier fields rather
// than reading them from state.
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
		Generation:         state.Generation,
		Active:             !state.Deactivated,
	}
}
