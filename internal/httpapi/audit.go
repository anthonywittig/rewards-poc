package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/audit"
	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// auditTimeout bounds the whole crawl: one GetWorkflowHistory round trip per
// run, walked serially because each run only learns its predecessor from the
// run it just read.
const auditTimeout = 30 * time.Second

// getAudit walks the customer's run chain -- back through
// ContinuedExecutionRunId until it reaches enrollment or runs out of history --
// and renders it newest-first. No worker is involved, so the audit page keeps
// working with the worker stopped.
func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}
	wfID := rewards.WorkflowID(id)

	ctx, cancel := context.WithTimeout(r.Context(), auditTimeout)
	defer cancel()

	// Describe first, to learn the current run ID: the crawl addresses every
	// run explicitly, because a failed read is how it detects that history was
	// reaped.
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, wfID, "")
	if err != nil {
		return mapStoreReadError(err)
	}

	crawled, err := audit.Build(
		ctx,
		audit.NewFetcher(s.temporal, wfID),
		id,
		desc.GetWorkflowExecutionInfo().GetExecution().GetRunId(),
	)
	if err != nil {
		return mapStoreReadError(err)
	}

	res := AuditResponse{
		Timeline: crawled,
		Entries:  make([]AuditEntry, 0, len(crawled.Entries)),
	}
	for _, e := range crawled.Entries {
		res.Entries = append(res.Entries, AuditEntry{
			Entry:      e,
			HistoryURL: s.ui.historyURL(crawled.WorkflowID, e.RunID),
		})
	}
	writeJSON(w, s.log, http.StatusOK, res)
	return nil
}
