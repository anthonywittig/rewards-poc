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

// The audit timeline, reconstructed by crawling Event History. PLAN.md 6.
//
// This is the endpoint the whole POC is arranged around. Every other read is
// served by something that looks like a database -- a Query against live
// workflow state, or a visibility index. This one is served by *the log itself*:
// the customer's history of point-adds is not stored anywhere, it is derived by
// replaying the events Temporal recorded because it had to, in order to run the
// workflow at all.
//
// It is also where the limits of "Temporal as the system of record" show. Closed
// runs get reaped, so the log is not durable in the way a table would be, and
// the crawl is O(runs x events) with no index to help it. Both are visible in
// the response rather than papered over: PLAN.md 6.3.

// auditTimeout bounds the whole crawl, which is the one endpoint whose cost
// grows with a customer's age -- one GetWorkflowHistory round trip per
// generation, walked serially because each run only learns its predecessor from
// the run it just read.
//
// Measured against the real stack: a 34-run customer (100 adds) crawls end to end
// in ~125ms, so this leaves better than two orders of magnitude of headroom and
// only ever fires on something pathological. Deliberately not a cap on runs walked -- a
// partial crawl that stopped early would have to report itself as Truncated,
// which in this contract means "history was deleted", and quietly redefining
// that would make the one honest signal in the response dishonest.
const auditTimeout = 30 * time.Second

// auditSubject names this endpoint in a timeout message. It exists because the
// default wording blames the worker, and the crawl never speaks to one --
// see mapStoreReadError.
const auditSubject = "the audit crawl"

// getAudit walks the customer's run chain newest-first and renders it oldest-first.
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
		return mapStoreReadError(err, auditSubject)
	}

	resp, err := s.crawl(ctx, wfID, id, desc.GetWorkflowExecutionInfo().GetExecution().GetRunId())
	if err != nil {
		return err
	}
	writeJSON(w, s.log, http.StatusOK, resp)
	return nil
}

// crawl walks back through ContinuedExecutionRunId until it reaches enrollment
// or runs out of history, then renders what it found.
//
// No worker is involved anywhere in here, which is worth noticing: the audit
// page keeps working with `make worker` stopped, unlike the detail page. That
// falls out of taking LifetimeEarnEvents from the newest run's start payload
// rather than from a Query -- see the note in assemble.
func (s *Server) crawl(ctx context.Context, wfID, customerID, runID string) (AuditResponse, error) {
	runs, truncated, err := walkRuns(ctx, s.fetchRun(wfID), runID)
	if err != nil {
		return AuditResponse{}, mapStoreReadError(err, auditSubject)
	}
	return assemble(customerID, runs, truncated), nil
}

// historyFetcher reads one run's events. A function rather than a method so the
// walk can be driven from a synthetic run chain in tests -- including the reaped
// one, which is otherwise reproducible only by running `make reap` and waiting
// out a server-side batch job.
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
			// customer's crawl rather than a failure. PLAN.md 6.3.
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

// assemble flattens the walked runs oldest-first and fills in the counts.
func assemble(customerID string, runs []runAudit, truncated bool) AuditResponse {
	out := AuditResponse{
		CustomerID: customerID,
		Entries:    []AuditEntry{}, // never null on the wire
		Truncated:  truncated,
		RunsWalked: len(runs),
	}
	if truncated && len(runs) > 0 {
		out.OldestRunID = runs[len(runs)-1].runID
	}

	for i := len(runs) - 1; i >= 0; i-- { // reverse: oldest first
		out.Entries = append(out.Entries, runs[i].entries...)
		out.ShownEarnEvents += runs[i].earnEvents
	}

	if len(runs) > 0 {
		// The lifetime total, without a Query and without needing history we may
		// no longer have. Every run starts with the carried CustomerState in its
		// WorkflowExecutionStarted input, and LifetimeEarnEvents in that payload
		// is the count as of the *start* of that run -- so the newest run's
		// starting count plus the adds inside it is the current total, whatever
		// happened to the runs before it.
		//
		// This is the continue-as-new payload doing the job PLAN.md 6.3 describes
		// for it: it is what lets a truncated log say "3 of 21" instead of just
		// showing three rows and hoping nobody asks. It also happens to be why
		// this endpoint needs no worker, where the detail page does.
		newest := runs[0]
		out.LifetimeEarnEvents = newest.startState.LifetimeEarnEvents + newest.earnEvents
	}
	return out
}

// fetchRun reads one run's events from the server.
//
// isLongPoll is false, which is load-bearing rather than a default: with it set,
// the iterator on a *running* workflow blocks waiting for events that have not
// happened yet, and the audit page for an active customer would hang instead of
// returning what exists now.
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
// completion are separate events (PLAN.md 6.2) and only together make a row: the
// request carries the amount and reason, the outcome carries the new balance or
// the rejection.
type pendingUpdate struct {
	name     string
	updateID string
	amount   int
	reason   string
	at       time.Time
	eventID  int64
}

// auditRun maps one run's events to audit entries.
//
// Pure: it takes events and returns rows, with no client and no I/O, so every
// case below is testable against a hand-built history -- including the ones that
// are awkward to produce on demand against a real server, like a rejection or a
// notification. The crawl around it is the only part that needs a live stack.
func auditRun(runID string, events []*historypb.HistoryEvent) runAudit {
	out := runAudit{runID: runID}
	pending := map[int64]pendingUpdate{}
	activities := map[int64]rewards.NotifyRequest{}
	dc := converter.GetDefaultDataConverter()

	for _, e := range events {
		switch e.GetEventType() {

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			a := e.GetWorkflowExecutionStartedEventAttributes()
			out.previousRunID = a.GetContinuedExecutionRunId()
			decodeArg(dc, a.GetInput(), &out.startState)

			// The generation boundary is recorded here, on the *successor's*
			// first event, rather than on the predecessor's
			// WorkflowExecutionContinuedAsNew. Both mark the same instant, but
			// only this side knows which generation is being entered -- and when
			// history has been reaped, this side is the one that still exists.
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
			p, paired := pending[a.GetAcceptedEventId()]
			delete(pending, a.GetAcceptedEventId())

			// A future Update handler must not render as a point-add. An
			// unpaired completion (name unknown) is still shown: dropping a row
			// that history clearly contains would be the worse failure.
			if paired && p.name != "" && p.name != rewards.UpdateAddPoints {
				continue
			}

			// Anchored to the *accepted* event, not this one. That is the event
			// PLAN.md 6.2 pairs on, it is when the customer actually made the
			// request, and it exists even for an update whose outcome never
			// landed -- so the row's identity does not depend on the half of the
			// pair that is allowed to be missing.
			entry := AuditEntry{
				At:         p.at,
				Generation: out.startState.Generation,
				RunID:      runID,
				EventID:    p.eventID,
				Amount:     p.amount,
				Reason:     p.reason,
				RequestID:  p.updateID,
			}
			if !paired {
				entry.At = e.GetEventTime().AsTime()
				entry.EventID = e.GetEventId()
				entry.RequestID = a.GetMeta().GetUpdateId()
			}

			if f := a.GetOutcome().GetFailure(); f != nil {
				// Handler rejections only. A validator rejection writes nothing
				// to history at all, so it can never appear here -- which is the
				// asymmetry PLAN.md 3.4 exists to demonstrate, and the reason
				// this timeline is not a record of every attempt.
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

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			a := e.GetActivityTaskScheduledEventAttributes()
			if a.GetActivityType().GetName() != rewards.ActivityNotifyCustomer {
				continue
			}
			var req rewards.NotifyRequest
			decodeArg(dc, a.GetInput(), &req)
			activities[e.GetEventId()] = req

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			// Emitted on completion rather than on scheduling, so the row says
			// "sent" only when it was. A notification that exhausted its retries
			// leaves a Scheduled event and a Failed one, and claiming that as
			// sent would make the audit log lie about the one thing it exists to
			// be believed about.
			a := e.GetActivityTaskCompletedEventAttributes()
			req, ok := activities[a.GetScheduledEventId()]
			if !ok {
				continue // some other Activity, or one we never saw scheduled
			}
			delete(activities, a.GetScheduledEventId())
			out.entries = append(out.entries, AuditEntry{
				Kind:          AuditNotificationSent,
				At:            e.GetEventTime().AsTime(),
				Generation:    out.startState.Generation,
				RunID:         runID,
				EventID:       e.GetEventId(),
				NotifiedLevel: req.Level,
			})

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCEL_REQUESTED:
			// The request, not WorkflowExecutionCanceled, because this is the
			// moment the customer asked. The two are a workflow task apart and
			// the second only exists if the workflow shut down cleanly.
			out.entries = append(out.entries, AuditEntry{
				Kind:       AuditDeactivated,
				At:         e.GetEventTime().AsTime(),
				Generation: out.startState.Generation,
				RunID:      runID,
				EventID:    e.GetEventId(),
			})
		}
	}
	return out
}

// decodeArg decodes the first payload into dst, reporting whether it worked.
//
// Best-effort throughout the crawl, like decodeSearchAttributes: a row with a
// missing amount still tells the reader that an add happened, whereas a failed
// request tells them nothing at all. The DataConverter is the client's default,
// which is why the API and worker share a module -- PLAN.md 6.2.
func decodeArg(dc converter.DataConverter, ps *commonpb.Payloads, dst any) bool {
	if len(ps.GetPayloads()) == 0 {
		return false
	}
	return dc.FromPayload(ps.GetPayloads()[0], dst) == nil
}
