// Package audit reconstructs a customer's timeline by crawling Temporal Event
// History. Point-adds are not saved anywhere else: they are derived from the
// events Temporal recorded in order to run the workflow at all.
package audit

import "time"

// Kind tags a row of the audit timeline. Stable strings: the UI narrows on these.
type Kind string

const (
	KindEnrolled       Kind = "enrolled"
	KindPointsAdded    Kind = "points_added"
	KindPointsRejected Kind = "points_rejected"
	KindRunRolled      Kind = "run_rolled"
	KindDeactivated    Kind = "deactivated"
)

// Entry is one row of the customer's history, reconstructed by crawling Event
// History. Which fields are set depends on Kind. points_rejected only covers
// handler-side rejections: a validator rejection wrote nothing to history, so
// it is invisible here by design.
type Entry struct {
	Kind      Kind      `json:"kind"`
	At        time.Time `json:"at"`
	RunNumber int       `json:"runNumber"`
	RunID     string    `json:"runId"`
	// History event ID, unique only within its run.
	EventID int64 `json:"eventId"`
	// Deep link to this entry's run in the Temporal UI, filled in by the
	// HTTP handler -- the crawl itself does not know where the UI lives.
	HistoryURL string `json:"historyUrl"`

	Amount    int    `json:"amount,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Balance   int    `json:"balance,omitempty"`
	Level     string `json:"level,omitempty"`
	Failure   string `json:"failure,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Response is the body of GET /api/customers/{id}/audit.
//
// Truncation is part of the contract, not an error: closed runs are reaped
// after retention, so the crawl walks back until history is gone and then says
// so, quantified -- the UI renders "Showing 7 of 23 point events."
type Response struct {
	CustomerID string `json:"customerId"`
	// The execution the entries were crawled from.
	WorkflowID string  `json:"workflowId"`
	Entries    []Entry `json:"entries"` // newest first
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
