// Package httpapi is the HTTP surface over the rewards workflow. It holds a
// Temporal Client and nothing else -- no database, no cache, no ORM.
package httpapi

import (
	"time"

	"github.com/anthonywittig/rewards-poc/internal/audit"
)

// EnrollRequest is the body of POST /api/customers.
type EnrollRequest struct {
	Name string `json:"name"`
}

// AddPointsRequest is the body of POST /api/customers/{id}/points.
//
// RequestID is the caller's idempotency key. It becomes the Temporal Update
// ID, which the server dedups within a run, and rides along in the Update
// argument so the workflow can carry it across continue-as-new and reject a
// retry that straddled the roll. Either way the retry answers 200 with the
// current balance, not a double-apply.
type AddPointsRequest struct {
	Amount    int    `json:"amount"`
	Reason    string `json:"reason"`
	RequestID string `json:"requestId,omitempty"`
}

// CustomerResponse is the customer detail payload.
type CustomerResponse struct {
	// Identity
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Balance and tier (level + climb brackets are derived from Points)
	Points     int    `json:"points"`
	Level      string `json:"level"`
	PrevTierAt int    `json:"prevTierAt"`
	NextTierAt int    `json:"nextTierAt"`

	// Membership lifecycle
	EnrolledAt time.Time `json:"enrolledAt"`
	Status     string    `json:"status"` // "active" | "deactivated"

	// Execution bookkeeping
	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	RunNumber          int `json:"runNumber"`
	// Advisory: from DescribeWorkflowExecution, not the Query; may lag by one
	// run right after a continue-as-new.
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
	// Identity
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`

	// Balance (no tier brackets on the list)
	Points int    `json:"points"`
	Level  string `json:"level"`

	// Membership lifecycle
	EnrolledAt time.Time `json:"enrolledAt"`
	Status     string    `json:"status"` // "active" | "deactivated"

	// Execution bookkeeping
	RunNumber int    `json:"runNumber"`
	RunID     string `json:"runId"`
}

// ListLimit caps GET /api/customers. There is no pagination: Temporal rejects
// ORDER BY, so "page 2" of an unordered set could overlap or skip rows. The
// list returns a small fixed slice, says how many matched, and pushes the
// caller to filter.
const ListLimit = 5

// CustomerListResponse is the body of GET /api/customers.
type CustomerListResponse struct {
	Items []CustomerListItem `json:"items"`
	// The cap that was applied, echoed so the UI does not hardcode it.
	Limit int `json:"limit"`
	// Customers matching Query, ignoring the limit.
	Total int `json:"total"`
	// True when Items is everything that matched.
	Complete bool `json:"complete"`
	// The filter the server built from the structured params, pasteable into
	// the Temporal UI as-is.
	Query string `json:"query,omitempty"`
	// A link opening the Temporal UI's workflow list pre-filled with Query,
	// built here so the client needs no Temporal UI configuration of its own.
	QueryURL string `json:"queryUrl"`
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

// AuditEntry is an audit.Entry with a Temporal UI deep link. embedding flattens
// into the same JSON shape the UI already expects.
type AuditEntry struct {
	audit.Entry
	HistoryURL string `json:"historyUrl"`
}

// AuditResponse is GET /api/customers/{id}/audit. Crawl fields come from
// audit.Timeline; Entries are the same rows with HistoryURL filled in.
type AuditResponse struct {
	audit.Timeline
	Entries []AuditEntry `json:"entries"`
}
