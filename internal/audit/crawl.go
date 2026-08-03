package audit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/converter"
)

// Build walks the run chain from runID and assembles the customer's Timeline.
func Build(ctx context.Context, fetch Fetcher, customerID, runID string) (Timeline, error) {
	runs, truncated, err := Walk(ctx, fetch, runID)
	if err != nil {
		return Timeline{}, err
	}
	return Assemble(customerID, runs, truncated), nil
}

// Walk follows the chain newest-first. truncated is true when a predecessor's
// history had been reaped (crawl stopped short of enrollment); false when it
// reached the first run.
func Walk(ctx context.Context, fetch Fetcher, runID string) (runs []Run, truncated bool, err error) {
	for runID != "" {
		var events []*historypb.HistoryEvent
		events, err = fetch(ctx, runID)
		if err != nil {
			if isHistoryGone(err) && len(runs) > 0 {
				return runs, true, nil
			}
			return nil, false, err
		}

		run := FromEvents(runID, events)
		runs = append(runs, run)
		runID = run.PreviousRunID
	}
	return runs, false, nil
}

// Assemble flattens the walked runs newest-first and fills in the counts.
func Assemble(customerID string, runs []Run, truncated bool) Timeline {
	out := Timeline{
		CustomerID: customerID,
		WorkflowID: rewards.WorkflowID(customerID),
		Entries:    []Entry{},
		Truncated:  truncated,
		RunsWalked: len(runs),
	}

	// runs are already newest-first; reverse within each run because
	// FromEvents records entries in history order (oldest first).
	for _, run := range runs {
		for _, e := range slices.Backward(run.Entries) {
			out.Entries = append(out.Entries, e)
		}
		out.ShownEarnEvents += run.EarnEvents
	}

	if len(runs) > 0 {
		out.OldestRunID = runs[len(runs)-1].RunID
		// LifetimeEarnEvents in a run's start payload is the count as of that
		// run's start, so the newest run's starting count plus the adds inside
		// it is the current total -- available even when older history is gone,
		// which is what lets a truncated log say "3 of 21".
		newest := runs[0]
		out.LifetimeEarnEvents = newest.StartState.LifetimeEarnEvents + newest.EarnEvents
	}
	return out
}

// acceptedUpdate is the payload from a WorkflowExecutionUpdateAccepted event,
// keyed by that accepted event id until the matching UpdateCompleted arrives.
// Request fields (amount, reason) come from Accepted; outcome (balance or
// failure) from Completed.
type acceptedUpdate struct {
	name     string
	updateID string
	amount   int
	reason   string
	at       time.Time
	eventID  int64
}

// FromEvents maps one run's events to audit entries.
func FromEvents(runID string, events []*historypb.HistoryEvent) Run {
	run := Run{RunID: runID}
	// Keyed by Accepted event id (same id UpdateCompleted.AcceptedEventId).
	accepted := map[int64]acceptedUpdate{}
	dc := converter.GetDefaultDataConverter()

	for _, e := range events {
		switch e.GetEventType() {

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			a := e.GetWorkflowExecutionStartedEventAttributes()
			run.PreviousRunID = a.GetContinuedExecutionRunId()
			decodeArg(dc, a.GetInput(), &run.StartState)

			kind := KindEnrolled
			if run.PreviousRunID != "" {
				kind = KindRunRolled
			}
			run.Entries = append(run.Entries, Entry{
				Kind:      kind,
				At:        e.GetEventTime().AsTime(),
				RunNumber: run.StartState.RunNumber,
				RunID:     runID,
				EventID:   e.GetEventId(),
			})

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED:
			a := e.GetWorkflowExecutionUpdateAcceptedEventAttributes()
			req := a.GetAcceptedRequest()
			acc := acceptedUpdate{
				name:     req.GetInput().GetName(),
				updateID: req.GetMeta().GetUpdateId(),
				at:       e.GetEventTime().AsTime(),
				eventID:  e.GetEventId(),
			}
			var args rewards.AddPointsRequest
			if decodeArg(dc, req.GetInput().GetArgs(), &args) {
				acc.amount, acc.reason = args.Amount, args.Reason
			}
			accepted[e.GetEventId()] = acc

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_COMPLETED:
			a := e.GetWorkflowExecutionUpdateCompletedEventAttributes()
			// Always paired: an Update accepted in run N completes in run N.
			acc := accepted[a.GetAcceptedEventId()]

			// The departure. A *failed* leave applied nothing (the handler
			// stages, then commits), so only a success draws a row.
			if acc.name == rewards.UpdateDeactivate {
				if a.GetOutcome().GetFailure() != nil {
					continue
				}
				run.Entries = append(run.Entries, Entry{
					Kind:      KindDeactivated,
					At:        acc.at,
					RunNumber: run.StartState.RunNumber,
					RunID:     runID,
					EventID:   acc.eventID,
					RequestID: acc.updateID,
				})
				continue
			}

			// Anchored to the accepted event: that is when the customer made
			// the request.
			entry := Entry{
				At:        acc.at,
				RunNumber: run.StartState.RunNumber,
				RunID:     runID,
				EventID:   acc.eventID,
				Amount:    acc.amount,
				Reason:    acc.reason,
				RequestID: acc.updateID,
			}

			if f := a.GetOutcome().GetFailure(); f != nil {
				// Handler rejections only: a validator rejection wrote nothing
				// to history, so it can never appear here.
				entry.Kind = KindPointsRejected
				entry.Failure = f.GetMessage()
			} else {
				var res rewards.AddPointsResult
				decodeArg(dc, a.GetOutcome().GetSuccess(), &res)
				entry.Kind = KindPointsAdded
				entry.Balance = res.Balance
				entry.Level = res.Level
				run.EarnEvents++
			}
			run.Entries = append(run.Entries, entry)
		}
	}
	return run
}

// decodeArg decodes the first payload into dst, reporting whether it worked.
// Best-effort: a row with a missing amount still says an add happened.
func decodeArg(dc converter.DataConverter, ps *commonpb.Payloads, dst any) bool {
	if len(ps.GetPayloads()) == 0 {
		return false
	}
	return dc.FromPayload(ps.GetPayloads()[0], dst) == nil
}

// isHistoryGone reports whether a run's Event History has been deleted after
// retention. The crawl detects truncation by this error.
//
// Measured against a real server, a reaped run answers *InvalidArgument* with
// "...may have passed retention period." -- so the type alone cannot decide
// it, and this is the one place in the codebase that matches on message text.
// If a server upgrade changes the wording this surfaces as a loud 500 rather
// than a quietly shorter timeline, which is the right direction to fail in.
func isHistoryGone(err error) bool {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var invalid *serviceerror.InvalidArgument
	if !errors.As(err, &invalid) {
		return false
	}
	return strings.Contains(strings.ToLower(invalid.Error()), "retention period")
}
