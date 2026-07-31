// Package httpapi is the HTTP surface over the rewards workflow.
//
// It holds a Temporal Client and nothing else -- no database, no cache, no ORM.
// That is the entire argument of this POC, so the absence should be obvious at a
// glance rather than buried. See PLAN.md 5.
package httpapi

import "time"

// EnrollRequest is the body of POST /api/customers.
type EnrollRequest struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
	Email      string `json:"email"`
}

// AddPointsRequest is the body of POST /api/customers/{id}/points.
//
// RequestID is the caller's idempotency key and becomes the Temporal Update ID.
// The UI should send a fresh UUID per click. Worth knowing what that does and
// does not buy: Update dedup is scoped to a single Run, so it does not survive
// continue-as-new, and a retry that straddles a rollover can still double-apply.
// Adequate for points, not for money. PLAN.md 12.3.
type AddPointsRequest struct {
	Amount    int    `json:"amount"`
	Reason    string `json:"reason"`
	RequestID string `json:"requestId,omitempty"`
}

// CustomerResponse is the customer detail payload.
//
// Status comes from the execution rather than from workflow state: a cancelled
// execution is a deactivated customer, and the workflow cannot report its own
// closure. PLAN.md 3.6.
//
// Assembled from two reads -- Describe for liveness, Query for state -- and a
// continue-as-new can land between them. When it does, Points and Generation
// come from the successor run while RunID still names its predecessor. At three
// adds per run that window is small but genuinely reachable.
//
// Left that way deliberately. Pinning the Query to the RunID from Describe would
// make the response internally consistent, but a Query against a run that has
// just rolled fails -- so the endpoint would start erroring during exactly the
// rollovers it is supposed to survive, trading a cosmetic mismatch for a real
// outage. Generation is the field to trust for "which run": it is read in the
// same snapshot as the balance. RunID is advisory, useful for pasting into the
// Temporal UI, and may lag by one run.
type CustomerResponse struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"`
	EnrolledAt time.Time `json:"enrolledAt"`

	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	Status string `json:"status"` // "active" | "deactivated"
	// Advisory, and may lag the rest of this struct by one run -- see above.
	RunID string `json:"runId"`
}

// AddPointsResponse is what a successful add returns.
//
// No "promoted" flag: the workflow does not report one, and deriving it here
// would mean reading the balance before the add, which races every other add in
// flight. Tier-crossing detection belongs inside the handler where it is
// atomic -- that is Phase 6 (PLAN.md 3.7).
type AddPointsResponse struct {
	Balance int    `json:"balance"`
	Level   string `json:"level"`
	EventID string `json:"eventId"`
}

// EnrollResponse is what a successful enrollment returns.
type EnrollResponse struct {
	CustomerID string `json:"customerId"`
	WorkflowID string `json:"workflowId"`
	RunID      string `json:"runId"`
}

// ErrorResponse is the single error shape every failing endpoint returns.
// Code is stable and machine-readable; Message is for humans.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the code and message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
