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

// The audit timeline, reconstructed by crawling Event History rather than read
// from a store: the customer's point-adds are not saved anywhere, they are
// derived from the events Temporal recorded in order to run the workflow at all.

// auditTimeout bounds the whole crawl, which is the one endpoint whose cost
// grows with a customer's age -- one GetWorkflowHistory round trip per
// generation, walked serially because each run only learns its predecessor from
// the run it just read. A 34-run customer (100 adds) crawls in ~125ms.
//
// Deliberately not a cap on runs walked: a partial crawl would have to report
// itself as Truncated, which in this contract means "history was deleted".
const auditTimeout = 30 * time.Second

// getAudit walks the customer's run chain newest-first -- back through
// ContinuedExecutionRunId until it reaches enrollment or runs out of history --
// and renders what it found, newest-first. No worker is involved, so the audit
// page keeps working with `make worker` stopped.
func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}
	wfID := rewards.WorkflowID(id)

	ctx, cancel := context.WithTimeout(r.Context(), auditTimeout)
	defer cancel()

	// Describe first, purely to learn the current run ID. The crawl has to
	// address every run explicitly, because a failed read is how it detects that
	// history was reaped: with an empty run ID the server helpfully resolves to
	// the latest run instead of reporting the specific run as gone.
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, wfID, "")
	if err != nil {
		// mapStoreReadError, not mapQueryError: no worker is involved in the
		// crawl, so a timeout must not blame one.
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

// historyFetcher reads one run's events. A function rather than a method so the
// walk can be driven from a synthetic run chain in tests -- including the reaped
// case, otherwise reproducible only by running `make reap` and waiting.
type historyFetcher func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error)

// walkRuns follows the chain newest-first, reporting whether it ended because
// history had been deleted rather than because it reached enrollment.
func walkRuns(ctx context.Context, fetch historyFetcher, runID string) ([]runAudit, bool, error) {
	var runs []runAudit // newest first
	for runID != "" {
		events, err := fetch(ctx, runID)
		if err != nil {
			// A run we were *told about* by its successor, whose history is gone.
			// That is reaping, and it is the expected end of a long-lived
			// customer's crawl rather than a failure.
			//
			// Only once we are past the first run, though: history missing for
			// the run Describe just handed us is not truncation, it is the
			// execution disappearing underneath the request, and reporting that
			// as a successful empty timeline would hide a real fault.
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

	// The walk already produced runs newest-first; auditRun built each run's
	// entries in history order, so only those need reversing to put the newest
	// event of the whole timeline first.
	for _, run := range runs {
		for i := len(run.entries) - 1; i >= 0; i-- {
			out.Entries = append(out.Entries, run.entries[i])
		}
		out.ShownEarnEvents += run.earnEvents
	}

	if len(runs) > 0 {
		// The lifetime total, without a Query and without needing history we may
		// no longer have. LifetimeEarnEvents in a run's start payload is the
		// count as of the *start* of that run, so the newest run's starting
		// count plus the adds inside it is the current total -- which is what
		// lets a truncated log say "3 of 21".
		newest := runs[0]
		out.LifetimeEarnEvents = newest.startState.LifetimeEarnEvents + newest.earnEvents
	}
	return out
}

// fetchRun reads one run's events from the server.
//
// isLongPoll is false, which is load-bearing rather than a default: with it set,
// the iterator on a *running* workflow blocks waiting for events that have not
// happened yet, so the audit page for an active customer would hang.
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
	// The CustomerState this run was started with -- the enrollment payload on
	// the first run, the carried state on every one after.
	startState rewards.CustomerState
	entries    []AuditEntry
	earnEvents int
}

// pendingUpdate is an accepted Update waiting for its outcome. Acceptance and
// completion are separate events and only together make a row: the request
// carries the amount and reason, the outcome carries the new balance or the
// rejection.
type pendingUpdate struct {
	name     string
	updateID string
	amount   int
	reason   string
	at       time.Time
	eventID  int64
}

// auditRun maps one run's events to audit entries. Pure -- no client, no I/O --
// so every case below is testable against a recorded history.
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

			// The generation boundary is recorded on the *successor's* first
			// event rather than the predecessor's ContinuedAsNew: only this side
			// knows which generation is being entered, and when history has been
			// reaped this side is the one that still exists.
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
			// Always paired: an Update accepted in run N completes in run N --
			// updates never survive a run boundary, which is the whole reason the
			// API retries across continue-as-new -- and this run's events were
			// read in full, in order, so the accepted event has been seen.
			p := pending[a.GetAcceptedEventId()]

			// The departure. Idempotent against a concurrent duplicate, so
			// history can hold a completion that changed nothing -- only the
			// real transition belongs on the timeline, or a raced DELETE reads
			// as a second departure.
			//
			// A *failed* one is dropped rather than rendered as a rejection row,
			// unlike a failed addPoints: the handler stages its change and
			// commits only once the upsert is issued, so a failed Update applied
			// nothing and there is no half-state to disclose.
			if p.name == rewards.UpdateDeactivate {
				if a.GetOutcome().GetFailure() != nil {
					continue
				}
				// Undecodable payload defaults to "changed": a row history
				// clearly contains is shown rather than dropped on a decoding
				// technicality.
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

			// Anchored to the *accepted* event, not this one: it is when the
			// customer made the request.
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
				// Handler rejections only. A validator rejection writes nothing
				// to history, so it can never appear here -- which is why this
				// timeline is not a record of every attempt.
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
// Best-effort: a row with a missing amount still tells the reader that an add
// happened. The DataConverter is the client's default, which is why the API and
// worker share a module.
func decodeArg(dc converter.DataConverter, ps *commonpb.Payloads, dst any) bool {
	if len(ps.GetPayloads()) == 0 {
		return false
	}
	return dc.FromPayload(ps.GetPayloads()[0], dst) == nil
}
