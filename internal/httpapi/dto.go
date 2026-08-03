// Package httpapi is the HTTP surface over the rewards workflow. It holds a
// Temporal Client and nothing else -- no database, no cache, no ORM.
package httpapi

import (
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// EnrollRequest is the body of POST /api/customers.
type EnrollRequest struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
}

// AddPointsRequest is the body of POST /api/customers/{id}/points.
//
// RequestID is the caller's idempotency key and becomes the Temporal Update ID;
// send a fresh UUID per click. Note dedup is scoped to a single Run, so a retry
// straddling a continue-as-new can still double-apply -- adequate for points,
// not for money.
type AddPointsRequest struct {
	Amount    int    `json:"amount"`
	Reason    string `json:"reason"`
	RequestID string `json:"requestId,omitempty"`
}

// CustomerResponse is the customer detail payload.
//
// Status comes from workflow state (Active / Deactivated), not from whether
// the Temporal execution is Running. Soft-inactive customers stay Running.
//
// Assembled from two reads -- Describe for liveness, Query for state -- and a
// continue-as-new can land between them, leaving RunID naming the predecessor
// while Points and Generation come from the successor. Generation is the field
// to trust for "which run"; RunID is advisory and may lag by one.
type CustomerResponse struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"`
	EnrolledAt time.Time `json:"enrolledAt"`

	// The whole ladder, ascending, so the UI can draw progress without holding
	// its own copy of the thresholds. NextTierAt alone is not enough: a bar also
	// needs the rung below the customer, and deriving that on the client means
	// hardcoding the ladder there -- which then goes stale silently, because a
	// wrong bar width renders perfectly.
	//
	// Answered by the worker, so these rungs are the ones Level and NextTierAt
	// above were actually derived from. Basic is not in it; the span from zero
	// to the first rung is implied. See rewards.Ladder.
	Tiers []rewards.Tier `json:"tiers"`

	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`

	Status string `json:"status"` // "active" | "deactivated"
	// Advisory, and may lag the rest of this struct by one run -- see above.
	RunID string `json:"runId"`
}

// AddPointsResponse is what a successful add returns. No "promoted" flag:
// tier-crossing detection happens inside the handler, where it is atomic.
type AddPointsResponse struct {
	Balance int    `json:"balance"`
	Level   string `json:"level"`
}

// EnrollResponse is what a successful enrollment returns.
type EnrollResponse struct {
	CustomerID string `json:"customerId"`
	WorkflowID string `json:"workflowId"`
	RunID      string `json:"runId"`
}

// CustomerListItem is one row of GET /api/customers.
//
// Narrower than CustomerResponse: the list is served by ListWorkflow, which
// returns *search attributes only*, so anything not registered as one is absent
// by construction -- most notably LifetimeEarnEvents. Use the detail endpoint.
type CustomerListItem struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	EnrolledAt time.Time `json:"enrolledAt"`
	Generation int       `json:"generation"`
	Status     string    `json:"status"` // "active" | "deactivated"
	RunID      string    `json:"runId"`
}

// ListLimit caps GET /api/customers. There is no pagination: Temporal rejects
// ORDER BY, so "page 2" of an unordered set could overlap or skip rows. The
// list returns a small fixed slice, says how many matched, and tells the user
// to filter.
const ListLimit = 5

// CustomerListResponse is the body of GET /api/customers.
//
// The UI renders one of:
//
//	Complete            -> no notice; this is everything matching
//	Total >= 0          -> "Showing 5 of 23 — filter to find additional results"
//	Total < 0           -> "Showing 5 of many — filter to find additional results"
//
// Three properties the UI has to respect, all consequences of the visibility
// store rather than of this API:
//
//   - **Results are unsorted.** Temporal rejects ORDER BY, so sorting is the
//     client's job -- and sorting five arbitrary rows out of twenty-three sorts
//     a sample, not the set. Only meaningful when Complete.
//   - **Which five you get is unspecified.** No ORDER BY means no stable
//     ordering, so the same request can return a different five.
//   - **Results lag writes.** Elasticsearch visibility is asynchronous,
//     ~200-300ms after tuning and never zero, so a just-created customer may be
//     missing from both Items and Total.
type CustomerListResponse struct {
	Items []CustomerListItem `json:"items"`
	// The cap that was applied, echoed so the UI does not hardcode it.
	Limit int `json:"limit"`
	// Customers matching Query, ignoring the limit. -1 when the count could not
	// be obtained, which is what "of many" is for. Comes from a separate
	// visibility query to Items, so the two can disagree by a row.
	Total int `json:"total"`
	// True when Items is everything that matched, i.e. Total <= Limit. Only
	// then is client-side sorting sorting the actual set.
	Complete bool `json:"complete"`
	// The filter the server built from the structured params, pasteable into
	// the Temporal UI as-is.
	Query string `json:"query,omitempty"`
}

// AuditEntryKind tags a row of the audit timeline. Stable strings: the UI
// narrows on these.
type AuditEntryKind string

const (
	AuditEnrolled         AuditEntryKind = "enrolled"
	AuditPointsAdded      AuditEntryKind = "points_added"
	AuditPointsRejected   AuditEntryKind = "points_rejected"
	AuditGenerationRolled AuditEntryKind = "generation_rolled"
	AuditDeactivated      AuditEntryKind = "deactivated"
	AuditReactivated      AuditEntryKind = "reactivated"
)

// AuditEntry is one row of the customer's history, reconstructed by crawling
// Event History.
//
// Which fields are populated depends on Kind:
//
//	enrolled           At, Generation, RunID
//	points_added       Amount, Reason, Balance, Level, RequestID
//	points_rejected    Amount, Reason, Failure, RequestID
//	generation_rolled  Generation (the one being entered)
//	deactivated        At, RunID
//
// Note points_rejected only ever covers *handler*-side rejections. A validator
// rejection writes nothing to history, so it is invisible here by design.
type AuditEntry struct {
	Kind       AuditEntryKind `json:"kind"`
	At         time.Time      `json:"at"`
	Generation int            `json:"generation"`
	RunID      string         `json:"runId"`
	// History event ID within its run. Unique only per run, so pair with RunID.
	EventID int64 `json:"eventId"`

	Amount    int    `json:"amount,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Balance   int    `json:"balance,omitempty"`
	Level     string `json:"level,omitempty"`
	Failure   string `json:"failure,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// AuditResponse is the body of GET /api/customers/{id}/audit.
//
// Truncation is a first-class part of this contract, not an error case. Closed
// runs get reaped, so the crawl walks backwards until history is gone and then
// says so; the carried CustomerState is what lets it *quantify* the gap rather
// than silently showing less. The UI renders "Showing 7 of 23 point events."
// from ShownEarnEvents and LifetimeEarnEvents.
type AuditResponse struct {
	CustomerID string `json:"customerId"`
	// The execution the entries were crawled from, so the UI can deep-link a run
	// into the Temporal UI without hardcoding rewards.WorkflowIDPrefix -- a
	// derivation that would go stale silently, since a wrong link still renders.
	WorkflowID string       `json:"workflowId"`
	Entries    []AuditEntry `json:"entries"` // newest first
	// True when the crawl hit a run whose history had been deleted.
	Truncated bool `json:"truncated"`
	// Point-add rows actually reconstructed, versus the lifetime count carried
	// in workflow state. Equal when Truncated is false.
	ShownEarnEvents    int `json:"shownEarnEvents"`
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	// The oldest run the crawl could read. Empty when it reached enrollment.
	OldestRunID string `json:"oldestRunId,omitempty"`
	// How many runs were walked, for the generation dividers.
	RunsWalked int `json:"runsWalked"`
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
