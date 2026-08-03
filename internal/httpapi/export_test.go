package httpapi

import "github.com/anthonywittig/rewards-poc/internal/rewards"

// This file exposes package internals to the external test package
// (httpapi_test). It is compiled only during tests, so nothing here widens
// the package's real API.

type (
	RunAudit       = runAudit
	HistoryFetcher = historyFetcher
	APIError       = apiError
	TemporalUI     = temporalUI
)

var (
	AuditRun        = auditRun
	WalkRuns        = walkRuns
	Assemble        = assemble
	NameTerms       = nameTerms
	BuildListFilter = buildListFilter
	NewTemporalUI   = newTemporalUI
)

// MakeRunAudit builds the runAudit values Assemble is tested against.
func MakeRunAudit(
	runID string, entries []AuditEntry, earnEvents int, startState rewards.CustomerState,
) runAudit {
	return runAudit{runID: runID, entries: entries, earnEvents: earnEvents, startState: startState}
}

func (r runAudit) RunID() string                     { return r.runID }
func (r runAudit) PreviousRunID() string             { return r.previousRunID }
func (r runAudit) StartState() rewards.CustomerState { return r.startState }
func (r runAudit) Entries() []AuditEntry             { return r.entries }
func (r runAudit) EarnEvents() int                   { return r.earnEvents }

func (e *apiError) Status() int  { return e.status }
func (e *apiError) Code() string { return e.code }

func (t temporalUI) HistoryURL(workflowID, runID string) string {
	return t.historyURL(workflowID, runID)
}
func (t temporalUI) QueryURL(query string) string { return t.queryURL(query) }
