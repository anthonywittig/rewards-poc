package audit

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/converter"
)

// Fetcher reads one run's events. A function rather than a client so the walk
// can be driven from a synthetic run chain in tests.
type Fetcher func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error)

// Run is one run's contribution to the timeline.
type Run struct {
	RunID         string
	PreviousRunID string
	// The CustomerState this run was started with.
	StartState rewards.CustomerState
	Entries    []Entry
	EarnEvents int
}

// Walk follows the chain newest-first, reporting whether it ended because
// history had been deleted rather than because it reached enrollment.
func Walk(ctx context.Context, fetch Fetcher, runID string) ([]Run, bool, error) {
	var runs []Run // newest first
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

		run := FromEvents(runID, events)
		runs = append(runs, run)
		runID = run.PreviousRunID
	}
	return runs, false, nil
}

// Assemble flattens the walked runs newest-first and fills in the counts.
func Assemble(customerID string, runs []Run, truncated bool) Response {
	out := Response{
		CustomerID: customerID,
		WorkflowID: rewards.WorkflowID(customerID),
		Entries:    []Entry{}, // never null on the wire
		Truncated:  truncated,
		RunsWalked: len(runs),
	}
	// Walk only reports truncation after reading at least one run, so the
	// index is safe.
	if truncated {
		out.OldestRunID = runs[len(runs)-1].RunID
	}

	for _, run := range runs {
		for i := len(run.Entries) - 1; i >= 0; i-- {
			out.Entries = append(out.Entries, run.Entries[i])
		}
		out.ShownEarnEvents += run.EarnEvents
	}

	if len(runs) > 0 {
		// LifetimeEarnEvents in a run's start payload is the count as of that
		// run's start, so the newest run's starting count plus the adds inside
		// it is the current total -- available even when older history is gone,
		// which is what lets a truncated log say "3 of 21".
		newest := runs[0]
		out.LifetimeEarnEvents = newest.StartState.LifetimeEarnEvents + newest.EarnEvents
	}
	return out
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

// FromEvents maps one run's events to audit entries. Pure -- no client, no
// I/O -- so it is testable against recorded histories.
func FromEvents(runID string, events []*historypb.HistoryEvent) Run {
	out := Run{RunID: runID}
	pending := map[int64]pendingUpdate{}
	dc := converter.GetDefaultDataConverter()

	for _, e := range events {
		switch e.GetEventType() {

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			a := e.GetWorkflowExecutionStartedEventAttributes()
			out.PreviousRunID = a.GetContinuedExecutionRunId()
			decodeArg(dc, a.GetInput(), &out.StartState)

			kind := KindEnrolled
			if out.PreviousRunID != "" {
				kind = KindRunRolled
			}
			out.Entries = append(out.Entries, Entry{
				Kind:      kind,
				At:        e.GetEventTime().AsTime(),
				RunNumber: out.StartState.RunNumber,
				RunID:     runID,
				EventID:   e.GetEventId(),
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
				out.Entries = append(out.Entries, Entry{
					Kind:      KindDeactivated,
					At:        p.at,
					RunNumber: out.StartState.RunNumber,
					RunID:     runID,
					EventID:   p.eventID,
					RequestID: p.updateID,
				})
				continue
			}

			// A future Update handler must not render as a point-add.
			if p.name != rewards.UpdateAddPoints {
				continue
			}

			// Anchored to the accepted event: that is when the customer made
			// the request.
			entry := Entry{
				At:        p.at,
				RunNumber: out.StartState.RunNumber,
				RunID:     runID,
				EventID:   p.eventID,
				Amount:    p.amount,
				Reason:    p.reason,
				RequestID: p.updateID,
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
				out.EarnEvents++
			}
			out.Entries = append(out.Entries, entry)
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
