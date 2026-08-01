// Package httpapi is the HTTP surface over the rewards workflow.
//
// It holds a Temporal Client and nothing else -- no database, no cache, no ORM.
// That is the entire argument of this POC, so the absence should be obvious at a
// glance rather than buried. See FINDINGS.md#the-http-api.
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
// The UI should send a fresh UUID per click. Worth knowing what that does and does
// not buy: Update dedup is scoped to a single Run, so it does not survive
// continue-as-new, and a retry that straddles a rollover can still double-apply.
// Adequate for points, not for money.
// FINDINGS.md#update-dedup-does-not-survive-continue-as-new.
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
// atomic -- that is Phase 6 (FINDINGS.md#tier-promotion-notifications).
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

// --- Frozen contract for Phases 4 and 5 -------------------------------------
//
// The types below started life so the UI could be shaped against a frozen
// contract before list/audit existed. They are still the wire types for those
// endpoints.

// CustomerListItem is one row of GET /api/customers.
//
// Deliberately narrower than CustomerResponse. The list is served by
// ListWorkflow, which returns *search attributes only* -- so anything not
// registered as one is absent here by construction, most notably
// LifetimeEarnEvents. Fetch the detail endpoint for that.
type CustomerListItem struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	EnrolledAt time.Time `json:"enrolledAt"`
	Generation int       `json:"generation"`
	Status     string    `json:"status"` // "active" | "deactivated"
	RunID      string    `json:"runId"`
}

// ListLimit caps GET /api/customers. There is no pagination.
//
// A deliberate simplification, and one the platform pushes towards: Temporal
// rejects ORDER BY (FINDINGS.md#order-by-is-not-supported), so a paginated list
// would hand back arbitrary pages of an unordered set — page 2 of "customers"
// means nothing in particular. Rather than build paging that cannot be made
// coherent, the list returns a small fixed slice, says how many matched in total,
// and tells the user to filter. Filtering is the operation the visibility store is
// actually good at.
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
//   - **Results are unsorted.** Temporal rejects ORDER BY outright, for custom
//     and built-in attributes alike, so sorting is the client's job. With no
//     pagination that is now always safe — Items is the whole of what was
//     returned — but note sorting five arbitrary rows out of twenty-three sorts
//     a sample, not the set. Only meaningful when Complete.
//   - **Which five you get is unspecified.** No ORDER BY means no stable
//     ordering, so the same request can return a different five. Do not build
//     anything that assumes otherwise.
//   - **Results lag writes.** Elasticsearch visibility is asynchronous,
//     ~200-300ms after tuning and never zero, so a just-created customer may be
//     missing from both Items and Total. FINDINGS.md#visibility-lag and 9.
type CustomerListResponse struct {
	Items []CustomerListItem `json:"items"`
	// The cap that was applied, echoed so the UI does not hardcode it.
	Limit int `json:"limit"`
	// Customers matching Query, ignoring the limit. -1 when the count could not
	// be obtained, which is what "of many" is for -- the list itself still
	// works, so a failed count degrades the message rather than the request.
	//
	// Total and Items come from two separate visibility queries, so under
	// concurrent writes they can disagree by a row. Not worth solving for a
	// count shown next to the word "filter".
	Total int `json:"total"`
	// True when Items is everything that matched, i.e. Total <= Limit. Only
	// then is client-side sorting sorting the actual set.
	Complete bool `json:"complete"`
	// Echoed back so the UI can show what it asked for, and so a rejected query
	// is debuggable from the response alone.
	Query string `json:"query,omitempty"`
}

// AuditEntryKind tags a row of the audit timeline. Stable strings: the UI
// narrows on these.
type AuditEntryKind string

const (
	AuditEnrolled         AuditEntryKind = "enrolled"
	AuditPointsAdded      AuditEntryKind = "points_added"
	AuditPointsRejected   AuditEntryKind = "points_rejected"
	AuditNotificationSent AuditEntryKind = "notification_sent"
	AuditGenerationRolled AuditEntryKind = "generation_rolled"
	AuditDeactivated      AuditEntryKind = "deactivated"
	AuditReactivated      AuditEntryKind = "reactivated"
)

// AuditEntry is one row of the customer's history, reconstructed by crawling
// Event History (FINDINGS.md#events-the-crawl-reads).
//
// A single flat struct with omitempty rather than a polymorphic union, because
// it crosses the wire as JSON and TypeScript narrows on Kind perfectly well.
// Which fields are populated depends on Kind:
//
//	enrolled           At, Generation, RunID
//	points_added       Amount, Reason, Balance, Level, RequestID
//	points_rejected    Amount, Reason, Failure, RequestID
//	notification_sent  NotifiedLevel
//	generation_rolled  Generation (the one being entered)
//	deactivated        At, RunID
//
// Note points_rejected only ever covers *handler*-side rejections. A validator
// rejection writes nothing to history at all, so it is invisible here by design --
// that asymmetry is the whole point of FINDINGS.md#the-validatorhandler-split.
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

	NotifiedLevel string `json:"notifiedLevel,omitempty"`
}

// AuditResponse is the body of GET /api/customers/{id}/audit.
//
// Truncation is a first-class part of this contract, not an error case. Closed
// runs get reaped, so the crawl walks backwards until history is gone and then
// says so -- and the carried CustomerState is what lets it *quantify* the gap
// rather than silently showing less. FINDINGS.md#truncation-detection.
//
// The UI renders "Showing 7 of 23 point events. Earlier history has been
// deleted." from ShownEarnEvents and LifetimeEarnEvents. Note the header of the
// detail page stays fully correct even when this is partial, because the totals
// ride in the continue-as-new payload rather than in history.
type AuditResponse struct {
	CustomerID string       `json:"customerId"`
	Entries    []AuditEntry `json:"entries"` // oldest first
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
