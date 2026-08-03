package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/converter"
)

// The audit timeline is reconstructed by crawling Event History rather than
// read from a store: point-adds are not saved anywhere else, they are derived
// from the events Temporal recorded in order to run the workflow at all.

// auditTimeout bounds the whole crawl: one GetWorkflowHistory round trip per
// generation, walked serially because each run only learns its predecessor
// from the run it just read.
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

	runs, truncated, err := walkRuns(ctx, s.fetchRun(wfID),
		desc.GetWorkflowExecutionInfo().GetExecution().GetRunId())
	if err != nil {
		return mapStoreReadError(err)
	}
	writeJSON(w, s.log, http.StatusOK, assemble(id, runs, truncated))
	return nil
}

// historyFetcher reads one run's events. A function rather than a method so
// the walk can be driven from a synthetic run chain in tests.
type historyFetcher func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error)

// walkRuns follows the chain newest-first, reporting whether it ended because
// history had been deleted rather than because it reached enrollment.
func walkRuns(ctx context.Context, fetch historyFetcher, runID string) ([]runAudit, bool, error) {
	var runs []runAudit // newest first
	for runID != "" {
		events, err := fetch(ctx, runID)
		if err != nil {
			// A predecessor whose history is gone: that is reaping, the
			// expected end of a long-lived customer's crawl. But only past the
			// first run -- the run Describe just handed us going missing is a
			// real fault, not truncation.
			if isHistoryGone(err) && len(runs) > 0 {
				return runs, true, nil
			}
			return nil, false, err
		}

		run := auditRun(runID, events)
		runs = append(runs, run)
		runID = run.previousRunID
	}
	return runs, false, nil
}

// assemble flattens the walked runs newest-first and fills in the counts.
func assemble(customerID string, runs []runAudit, truncated bool) AuditResponse {
	out := AuditResponse{
		CustomerID: customerID,
		WorkflowID: rewards.WorkflowID(customerID),
		Entries:    []AuditEntry{}, // never null on the wire
		Truncated:  truncated,
		RunsWalked: len(runs),
	}
	// walkRuns only reports truncation after reading at least one run, so the
	// index is safe.
	if truncated {
		out.OldestRunID = runs[len(runs)-1].runID
	}

	for _, run := range runs {
		for i := len(run.entries) - 1; i >= 0; i-- {
			out.Entries = append(out.Entries, run.entries[i])
		}
		out.ShownEarnEvents += run.earnEvents
	}

	if len(runs) > 0 {
		// LifetimeEarnEvents in a run's start payload is the count as of that
		// run's start, so the newest run's starting count plus the adds inside
		// it is the current total -- available even when older history is gone,
		// which is what lets a truncated log say "3 of 21".
		newest := runs[0]
		out.LifetimeEarnEvents = newest.startState.LifetimeEarnEvents + newest.earnEvents
	}
	return out
}

// fetchRun reads one run's events from the server. isLongPoll must be false:
// with it set, the iterator on a running workflow blocks waiting for future
// events and the audit page for an active customer would hang.
func (s *Server) fetchRun(wfID string) historyFetcher {
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

// runAudit is one run's contribution to the timeline.
type runAudit struct {
	runID         string
	previousRunID string
	// The CustomerState this run was started with.
	startState rewards.CustomerState
	entries    []AuditEntry
	earnEvents int
}

// pendingUpdate is an accepted Update waiting for its outcome: the request
// carries the amount and reason, the outcome the new balance or the rejection.
type pendingUpdate struct {
	name     string
	updateID string
	amount   int
	reason   string
	at       time.Time
	eventID  int64
}

// auditRun maps one run's events to audit entries. Pure -- no client, no I/O --
// so it is testable against recorded histories.
func auditRun(runID string, events []*historypb.HistoryEvent) runAudit {
	out := runAudit{runID: runID}
	pending := map[int64]pendingUpdate{}
	dc := converter.GetDefaultDataConverter()

	for _, e := range events {
		switch e.GetEventType() {

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			a := e.GetWorkflowExecutionStartedEventAttributes()
			out.previousRunID = a.GetContinuedExecutionRunId()
			decodeArg(dc, a.GetInput(), &out.startState)

			kind := AuditEnrolled
			if out.previousRunID != "" {
				kind = AuditGenerationRolled
			}
			out.entries = append(out.entries, AuditEntry{
				Kind:       kind,
				At:         e.GetEventTime().AsTime(),
				Generation: out.startState.Generation,
				RunID:      runID,
				EventID:    e.GetEventId(),
			})

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED:
			a := e.GetWorkflowExecutionUpdateAcceptedEventAttributes()
			req := a.GetAcceptedRequest()
			p := pendingUpdate{
				name:     req.GetInput().GetName(),
				updateID: req.GetMeta().GetUpdateId(),
				at:       e.GetEventTime().AsTime(),
				eventID:  e.GetEventId(),
			}
			var args rewards.AddPointsRequest
			if decodeArg(dc, req.GetInput().GetArgs(), &args) {
				p.amount, p.reason = args.Amount, args.Reason
			}
			pending[e.GetEventId()] = p

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_COMPLETED:
			a := e.GetWorkflowExecutionUpdateCompletedEventAttributes()
			// Always paired: an Update accepted in run N completes in run N.
			p := pending[a.GetAcceptedEventId()]

			// The departure. Only the real transition draws a row: a raced
			// duplicate completes with Changed=false, and a *failed* leave
			// applied nothing (the handler stages, then commits), so neither
			// belongs on the timeline.
			if p.name == rewards.UpdateDeactivate {
				if a.GetOutcome().GetFailure() != nil {
					continue
				}
				// Undecodable payload defaults to "changed": a row history
				// clearly contains is shown rather than dropped.
				changed := true
				var res rewards.DeactivateResult
				if decodeArg(dc, a.GetOutcome().GetSuccess(), &res) {
					changed = res.Changed
				}
				if !changed {
					continue
				}
				out.entries = append(out.entries, AuditEntry{
					Kind:       AuditDeactivated,
					At:         p.at,
					Generation: out.startState.Generation,
					RunID:      runID,
					EventID:    p.eventID,
					RequestID:  p.updateID,
				})
				continue
			}

			// A future Update handler must not render as a point-add.
			if p.name != rewards.UpdateAddPoints {
				continue
			}

			// Anchored to the accepted event: that is when the customer made
			// the request.
			entry := AuditEntry{
				At:         p.at,
				Generation: out.startState.Generation,
				RunID:      runID,
				EventID:    p.eventID,
				Amount:     p.amount,
				Reason:     p.reason,
				RequestID:  p.updateID,
			}

			if f := a.GetOutcome().GetFailure(); f != nil {
				// Handler rejections only: a validator rejection wrote nothing
				// to history, so it can never appear here.
				entry.Kind = AuditPointsRejected
				entry.Failure = f.GetMessage()
			} else {
				var res rewards.AddPointsResult
				decodeArg(dc, a.GetOutcome().GetSuccess(), &res)
				entry.Kind = AuditPointsAdded
				entry.Balance = res.Balance
				entry.Level = res.Level
				out.earnEvents++
			}
			out.entries = append(out.entries, entry)
		}
	}
	return out
}

// decodeArg decodes the first payload into dst, reporting whether it worked.
// Best-effort: a row with a missing amount still says an add happened.
func decodeArg(dc converter.DataConverter, ps *commonpb.Payloads, dst any) bool {
	if len(ps.GetPayloads()) == 0 {
		return false
	}
	return dc.FromPayload(ps.GetPayloads()[0], dst) == nil
}
