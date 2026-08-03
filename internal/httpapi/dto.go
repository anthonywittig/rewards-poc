// Package httpapi is the HTTP surface over the rewards workflow. It holds a
// Temporal Client and nothing else -- no database, no cache, no ORM.
package httpapi

import (
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// EnrollRequest is the body of POST /api/customers. CustomerID is optional;
// without it the server derives one from the name.
type EnrollRequest struct {
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
}

// AddPointsRequest is the body of POST /api/customers/{id}/points.
//
// RequestID is the caller's idempotency key and becomes the Temporal Update
// ID. Dedup is scoped to a single run, so a retry straddling a
// continue-as-new can still double-apply -- adequate for points, not money.
type AddPointsRequest struct {
	Amount    int    `json:"amount"`
	Reason    string `json:"reason"`
	RequestID string `json:"requestId,omitempty"`
}

// CustomerResponse is the customer detail payload. Status needs both reads to
// agree: a customer is active only while the execution is Running and workflow
// state says Active. Deactivation completes the workflow, so a departed
// customer's run is closed.
type CustomerResponse struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"`
	EnrolledAt time.Time `json:"enrolledAt"`

	// The whole ladder, ascending, so the UI can draw progress without its own
	// copy of the thresholds.
	Tiers []rewards.Tier `json:"tiers"`

	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	RunNumber          int `json:"runNumber"`

	Status string `json:"status"` // "active" | "deactivated"
	// Advisory: assembled from a separate read than the fields above, so it
	// may lag by one run right after a continue-as-new.
	RunID string `json:"runId"`
}

// AddPointsResponse is what a successful add returns.
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

// CustomerListItem is one row of GET /api/customers. Narrower than
// CustomerResponse because the list is served from search attributes only.
type CustomerListItem struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	EnrolledAt time.Time `json:"enrolledAt"`
	RunNumber  int       `json:"runNumber"`
	Status     string    `json:"status"` // "active" | "deactivated"
	RunID      string    `json:"runId"`
}

// ListLimit caps GET /api/customers. There is no pagination: Temporal rejects
// ORDER BY, so "page 2" of an unordered set could overlap or skip rows. The
// list returns a small fixed slice, says how many matched, and pushes the
// caller to filter.
const ListLimit = 5

// CustomerListResponse is the body of GET /api/customers.
//
// Properties the UI has to respect, all consequences of the visibility store:
// results come back in an unspecified order (rendered as-is), which rows you
// get is unspecified, and results lag writes by a few hundred ms.
type CustomerListResponse struct {
	Items []CustomerListItem `json:"items"`
	// The cap that was applied, echoed so the UI does not hardcode it.
	Limit int `json:"limit"`
	// Customers matching Query, ignoring the limit. -1 when the count could
	// not be obtained.
	Total int `json:"total"`
	// True when Items is everything that matched.
	Complete bool `json:"complete"`
	// The filter the server built from the structured params, pasteable into
	// the Temporal UI as-is.
	Query string `json:"query,omitempty"`
}

// AuditEntryKind tags a row of the audit timeline. Stable strings: the UI
// narrows on these.
type AuditEntryKind string

const (
	AuditEnrolled       AuditEntryKind = "enrolled"
	AuditPointsAdded    AuditEntryKind = "points_added"
	AuditPointsRejected AuditEntryKind = "points_rejected"
	AuditRunRolled      AuditEntryKind = "run_rolled"
	AuditDeactivated    AuditEntryKind = "deactivated"
)

// AuditEntry is one row of the customer's history, reconstructed by crawling
// Event History. Which fields are set depends on Kind. points_rejected only
// covers handler-side rejections: a validator rejection wrote nothing to
// history, so it is invisible here by design.
type AuditEntry struct {
	Kind      AuditEntryKind `json:"kind"`
	At        time.Time      `json:"at"`
	RunNumber int            `json:"runNumber"`
	RunID     string         `json:"runId"`
	// History event ID, unique only within its run.
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
// Truncation is part of the contract, not an error: closed runs are reaped
// after retention, so the crawl walks back until history is gone and then says
// so, quantified -- the UI renders "Showing 7 of 23 point events."
type AuditResponse struct {
	CustomerID string `json:"customerId"`
	// The execution the entries were crawled from, so the UI can deep-link
	// into the Temporal UI.
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
	// How many runs were walked, for the run dividers.
	RunsWalked int `json:"runsWalked"`
}

// ErrorResponse is the single error shape every failing endpoint returns.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the stable code and the human-readable message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
