package audit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/audit"
	"github.com/anthonywittig/rewards-poc/internal/rewards"

	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	updatepb "go.temporal.io/api/update/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The golden files in testdata/ are `temporal workflow show -o json` output
// from the local stack, one file per run of one customer's life. The
// generation->runNumber rename (0-based generation became the 1-based
// runNumber) was applied mechanically to the recorded payloads and search
// attributes rather than recapturing every file:
//
//	run-enrollment.json    the first run: enrollment + 3 adds, then the roll
//	run-continued.json     a middle run: rolled into, 3 adds, rolled out of
//	run-deactivated.json   the last run: rolled into, 1 add, then a deactivate
//	run-rejection.json     a run containing a handler rejection at the cap
//
// Testing against recorded server output rather than hand-built protos is
// deliberate: a synthetic history only proves the code agrees with whoever
// wrote the test.

func loadEvents(t *testing.T, name string) []*historypb.HistoryEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var h historypb.History
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &h); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	if len(h.GetEvents()) == 0 {
		t.Fatalf("%s decoded to zero events", name)
	}
	return h.GetEvents()
}

// membershipUpdate builds the Accepted/Completed pair Temporal writes for a
// deactivate Update. Built rather than captured because the cases that matter
// are the variations -- a raced duplicate, a failed leave; it mirrors the real
// captured pair at the end of run-deactivated.json.
func membershipUpdate(
	t *testing.T, firstEventID int64, name, updateID string, result any,
) []*historypb.HistoryEvent {
	t.Helper()
	payload, err := converter.GetDefaultDataConverter().ToPayloads(result)
	if err != nil {
		t.Fatalf("encode %s result: %v", name, err)
	}
	at := timestamppb.New(time.Date(2026, 7, 31, 20, 39, 43, 0, time.UTC))

	return []*historypb.HistoryEvent{
		{
			EventId:   firstEventID,
			EventTime: at,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionUpdateAcceptedEventAttributes{
				WorkflowExecutionUpdateAcceptedEventAttributes: &historypb.WorkflowExecutionUpdateAcceptedEventAttributes{
					ProtocolInstanceId: updateID,
					AcceptedRequest: &updatepb.Request{
						Meta:  &updatepb.Meta{UpdateId: updateID},
						Input: &updatepb.Input{Name: name},
					},
				},
			},
		},
		{
			EventId:   firstEventID + 1,
			EventTime: at,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_COMPLETED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionUpdateCompletedEventAttributes{
				WorkflowExecutionUpdateCompletedEventAttributes: &historypb.WorkflowExecutionUpdateCompletedEventAttributes{
					Meta:            &updatepb.Meta{UpdateId: updateID},
					AcceptedEventId: firstEventID,
					Outcome: &updatepb.Outcome{
						Value: &updatepb.Outcome_Success{Success: payload},
					},
				},
			},
		},
	}
}

func kinds(entries []audit.Entry) []audit.Kind {
	out := make([]audit.Kind, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

func requireKinds(t *testing.T, got []audit.Entry, want ...audit.Kind) {
	t.Helper()
	g := kinds(got)
	if len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v, want %v", g[i], want[i])
		}
	}
}

// The first run of a customer's life: the only one whose start event has no
// predecessor, and therefore the only one that renders as an enrollment.
func TestFromEvents_EnrollmentRun(t *testing.T) {
	run := audit.FromEvents("run-0", loadEvents(t, "run-enrollment.json"))

	if run.PreviousRunID != "" {
		t.Errorf("PreviousRunID = %q, want empty on the enrollment run", run.PreviousRunID)
	}
	if run.StartState.RunNumber != 1 {
		t.Errorf("runNumber = %d, want 1 (the enrollment run)", run.StartState.RunNumber)
	}
	requireKinds(t, run.Entries,
		audit.KindEnrolled, audit.KindPointsAdded, audit.KindPointsAdded, audit.KindPointsAdded)
	if run.EarnEvents != 3 {
		t.Errorf("EarnEvents = %d, want 3 (EarnsPerRun)", run.EarnEvents)
	}

	// The request half of the row comes from the accepted event, the outcome
	// half from the completed one.
	add := run.Entries[1]
	if add.Amount != 1000 || add.Reason == "" {
		t.Errorf("request side not decoded: amount=%d reason=%q", add.Amount, add.Reason)
	}
	if add.Balance != 1000 {
		t.Errorf("outcome side not decoded: balance=%d", add.Balance)
	}
	if add.RequestID == "" {
		t.Error("RequestID should carry the Update ID")
	}
}

// A run that was rolled into. The run divider is recorded here rather
// than on the predecessor's ContinuedAsNew event, so it survives the
// predecessor being reaped.
func TestFromEvents_ContinuedRun(t *testing.T) {
	run := audit.FromEvents("run-1", loadEvents(t, "run-continued.json"))

	if run.PreviousRunID == "" {
		t.Fatal("PreviousRunID should name the predecessor run")
	}
	requireKinds(t, run.Entries,
		audit.KindRunRolled, audit.KindPointsAdded, audit.KindPointsAdded, audit.KindPointsAdded)

	if run.Entries[0].RunNumber != 2 {
		t.Errorf("divider runNumber = %d, want 2 (the run being entered)",
			run.Entries[0].RunNumber)
	}
	// Carried state is all this run knows about its predecessors.
	if run.StartState.LifetimeEarnEvents != 3 {
		t.Errorf("carried lifetimeEarnEvents = %d, want 3", run.StartState.LifetimeEarnEvents)
	}
	if run.StartState.Points != 3000 {
		t.Errorf("carried points = %d, want 3000", run.StartState.Points)
	}
}

// Deactivate is an Update, so it appears as an Accepted/Completed pair rather
// than a CancelRequested event.
func TestFromEvents_DeactivatedRun(t *testing.T) {
	run := audit.FromEvents("run-2", loadEvents(t, "run-deactivated.json"))

	requireKinds(t, run.Entries,
		audit.KindRunRolled, audit.KindPointsAdded, audit.KindDeactivated)
}

// A deactivate that changed nothing -- a duplicate that raced the real leave
// into the same run -- still completes and still writes history. Rendering it
// would show the customer leaving twice.
func TestFromEvents_NoOpDeactivateDrawsNoRow(t *testing.T) {
	events := loadEvents(t, "run-deactivated.json")
	events = append(events, membershipUpdate(t, 200, rewards.UpdateDeactivate, "repeat-delete",
		rewards.DeactivateResult{Changed: false})...)

	run := audit.FromEvents("run-2", events)

	requireKinds(t, run.Entries,
		audit.KindRunRolled, audit.KindPointsAdded, audit.KindDeactivated)
}

// The failure path. The handler stages its change and commits only once the
// search attribute upsert is issued, so a failed deactivate applied nothing --
// and unlike a failed addPoints, there is no half-state to disclose.
func TestFromEvents_FailedMembershipUpdateDrawsNoRow(t *testing.T) {
	events := loadEvents(t, "run-deactivated.json")
	pair := membershipUpdate(t, 200, rewards.UpdateDeactivate, "leave-failed",
		rewards.DeactivateResult{Changed: true})
	pair[1].GetWorkflowExecutionUpdateCompletedEventAttributes().Outcome = &updatepb.Outcome{
		Value: &updatepb.Outcome_Failure{
			Failure: &failurepb.Failure{Message: "upsert search attributes: boom"},
		},
	}

	run := audit.FromEvents("run-2", append(events, pair...))

	requireKinds(t, run.Entries,
		audit.KindRunRolled, audit.KindPointsAdded, audit.KindDeactivated)
}

// The recorded half of the validator/handler split: a handler rejection
// becomes a row; a validator rejection wrote nothing and can never appear.
func TestFromEvents_HandlerRejectionIsRecorded(t *testing.T) {
	run := audit.FromEvents("cap-run", loadEvents(t, "run-rejection.json"))

	requireKinds(t, run.Entries,
		audit.KindRunRolled, audit.KindPointsAdded, audit.KindPointsRejected)

	rejected := run.Entries[2]
	if rejected.Failure == "" {
		t.Error("rejection row must carry the workflow's own message")
	}
	if rejected.Amount != 500 {
		t.Errorf("the attempted amount = %d, want 500", rejected.Amount)
	}

	// A rejection is not an earn; counting it would make an intact log look
	// truncated against the carried lifetime count.
	if run.EarnEvents != 1 {
		t.Errorf("EarnEvents = %d, want 1", run.EarnEvents)
	}
}

// --- the walk ---------------------------------------------------------------

// fakeChain serves a synthetic run chain, answering NotFound for reaped runs
// the way the server does.
func fakeChain(runs map[string][]*historypb.HistoryEvent) audit.Fetcher {
	return func(_ context.Context, runID string) ([]*historypb.HistoryEvent, error) {
		events, ok := runs[runID]
		if !ok {
			return nil, serviceerror.NewNotFound("workflow execution not found")
		}
		return events, nil
	}
}

func TestWalk_StopsAtEnrollment(t *testing.T) {
	enrollment := loadEvents(t, "run-enrollment.json")
	continued := loadEvents(t, "run-continued.json")
	prev := continued[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()

	runs, truncated, err := audit.Walk(context.Background(),
		fakeChain(map[string][]*historypb.HistoryEvent{"newest": continued, prev: enrollment}),
		"newest")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if truncated {
		t.Error("a chain walked back to enrollment is not truncated")
	}
	if len(runs) != 2 {
		t.Fatalf("walked %d runs, want 2", len(runs))
	}
}

// The truncation case: the predecessor was reaped, so the walk cannot reach
// enrollment and must say so.
func TestWalk_TruncatedWhenPredecessorReaped(t *testing.T) {
	continued := loadEvents(t, "run-continued.json")

	runs, truncated, err := audit.Walk(context.Background(),
		fakeChain(map[string][]*historypb.HistoryEvent{"newest": continued}), // predecessor absent
		"newest")
	if err != nil {
		t.Fatalf("a reaped predecessor is expected, not an error: %v", err)
	}
	if !truncated {
		t.Fatal("walk must report truncation when a named predecessor is gone")
	}
	if len(runs) != 1 {
		t.Fatalf("walked %d runs, want 1", len(runs))
	}
}

// A NotFound on the very first run is a real fault, not truncation -- Describe
// just resolved that run.
func TestWalk_FirstRunNotFoundIsAnError(t *testing.T) {
	_, _, err := audit.Walk(context.Background(),
		fakeChain(map[string][]*historypb.HistoryEvent{}), "gone")

	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want a NotFound to propagate", err)
	}
}

// --- assembly ---------------------------------------------------------------

func TestAssemble_NewestFirst(t *testing.T) {
	runs := []audit.Run{ // newest first, as the walk produces them
		{
			RunID: "b",
			Entries: []audit.Entry{
				{Kind: audit.KindRunRolled, RunID: "b"},
				{Kind: audit.KindPointsAdded, RunID: "b"},
			},
			EarnEvents: 1,
			StartState: rewards.CustomerState{LifetimeEarnEvents: 2},
		},
		{
			RunID: "a",
			Entries: []audit.Entry{
				{Kind: audit.KindEnrolled, RunID: "a"},
			},
			EarnEvents: 2,
		},
	}

	got := audit.Assemble("ada", runs, false)
	// Newest run first, and within it the newest event first.
	requireKinds(t, got.Entries, audit.KindPointsAdded, audit.KindRunRolled, audit.KindEnrolled)
	if got.RunsWalked != 2 {
		t.Errorf("runsWalked = %d, want 2", got.RunsWalked)
	}
	if got.OldestRunID != "a" {
		t.Errorf("oldestRunId = %q, want the enrollment run", got.OldestRunID)
	}
}

// History reaped: the log is short but the lifetime count is still right,
// which is what lets the UI say "Showing 3 of 21 point events."
func TestAssemble_LifetimeSurvivesTruncation(t *testing.T) {
	runs := []audit.Run{
		{RunID: "run7", EarnEvents: 2, StartState: rewards.CustomerState{LifetimeEarnEvents: 19}},
		{RunID: "run6", EarnEvents: 1, StartState: rewards.CustomerState{LifetimeEarnEvents: 18}},
	}

	got := audit.Assemble("grace", runs, true)
	if got.ShownEarnEvents != 3 {
		t.Errorf("shown = %d, want 3", got.ShownEarnEvents)
	}
	if got.LifetimeEarnEvents != 21 {
		t.Errorf("lifetime = %d, want 21 -- it comes from carried state, not the rows",
			got.LifetimeEarnEvents)
	}
	if got.OldestRunID != "run6" {
		t.Errorf("oldestRunId = %q, want the oldest run actually read", got.OldestRunID)
	}
}

// --- shape of the whole thing ------------------------------------------------

// End to end over the golden chain: the three runs of one customer's life,
// rendered as one timeline, newest first.
func TestCrawlShape_WholeCustomerLife(t *testing.T) {
	deactivated := loadEvents(t, "run-deactivated.json")
	continued := loadEvents(t, "run-continued.json")
	enrollment := loadEvents(t, "run-enrollment.json")

	run2 := deactivated[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()
	run1 := continued[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()

	runs, truncated, err := audit.Walk(context.Background(), fakeChain(map[string][]*historypb.HistoryEvent{
		"run3": deactivated, run2: continued, run1: enrollment,
	}), "run3")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	got := audit.Assemble("hist", runs, truncated)
	requireKinds(t, got.Entries,
		audit.KindDeactivated, audit.KindPointsAdded, audit.KindRunRolled,
		audit.KindPointsAdded, audit.KindPointsAdded, audit.KindPointsAdded, audit.KindRunRolled,
		audit.KindPointsAdded, audit.KindPointsAdded, audit.KindPointsAdded, audit.KindEnrolled)

	if got.ShownEarnEvents != 7 || got.LifetimeEarnEvents != 7 {
		t.Errorf("shown=%d lifetime=%d, want both 7", got.ShownEarnEvents, got.LifetimeEarnEvents)
	}
	if got.Truncated {
		t.Error("an intact chain must not report truncation")
	}
}
