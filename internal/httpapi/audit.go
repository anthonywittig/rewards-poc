package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/audit"
	"github.com/anthonywittig/rewards-poc/internal/rewards"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
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

	runs, truncated, err := audit.Walk(ctx, s.fetchRun(wfID),
		desc.GetWorkflowExecutionInfo().GetExecution().GetRunId())
	if err != nil {
		return mapStoreReadError(err)
	}

	res := audit.Assemble(id, runs, truncated)
	// Links are the handler's concern: assemble stays pure and testable
	// against recorded histories with no UI configuration in the fixtures.
	for i := range res.Entries {
		res.Entries[i].HistoryURL = s.ui.historyURL(res.WorkflowID, res.Entries[i].RunID)
	}
	writeJSON(w, s.log, http.StatusOK, res)
	return nil
}

// fetchRun reads one run's events from the server. isLongPoll must be false:
// with it set, the iterator on a running workflow blocks waiting for future
// events and the audit page for an active customer would hang.
func (s *Server) fetchRun(wfID string) audit.Fetcher {
	return func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error) {
		iter := s.temporal.GetWorkflowHistory(ctx, wfID, runID, false,
			enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

		var events []*historypb.HistoryEvent
		for iter.HasNext() {
			e, err := iter.Next()
			if err != nil {
				return nil, err
			}
			events = append(events, e)
		}
		return events, nil
	}
}
