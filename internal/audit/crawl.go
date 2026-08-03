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
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// Fetcher reads one run's events.
type Fetcher func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error)

// HistorySource is the GetWorkflowHistory half of a Temporal client.
type HistorySource interface {
	GetWorkflowHistory(
		ctx context.Context,
		workflowID string,
		runID string,
		isLongPoll bool,
		filterType enumspb.HistoryEventFilterType,
	) client.HistoryEventIterator
}

// NewFetcher reads runs of workflowID via the Temporal client.
// isLongPoll is always false so a live workflow does not hang the crawl
// waiting for future events.
func NewFetcher(src HistorySource, workflowID string) Fetcher {
	return func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error) {
		iter := src.GetWorkflowHistory(ctx, workflowID, runID, false,
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

type Run struct {
	RunID         string
	PreviousRunID string
	// The CustomerState this run was started with.
	StartState rewards.CustomerState
	Entries    []Entry
	EarnEvents int
}

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

// FromEvents maps one run's events to audit entries.
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

			// The departure. A *failed* leave applied nothing (the handler
			// stages, then commits), so only a success draws a row.
			if p.name == rewards.UpdateDeactivate {
				if a.GetOutcome().GetFailure() != nil {
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
