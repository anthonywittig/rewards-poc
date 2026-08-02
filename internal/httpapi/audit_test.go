package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// The golden files in testdata/ are `temporal workflow show -o json` output from
// the local stack, one file per run. Verbatim but for one edit: the customer
// contact details those runs carried have been stripped from the recorded
// payloads and search attributes, since nothing reads them any more. One file
// per run:
//
//	run-enrollment.json    the first run: enrollment + 3 adds, then the roll
//	run-continued.json     a middle run: rolled into, 3 adds, rolled out of
//	run-deactivated.json   the last run: rolled into, 1 add, then (historically) cancelled;
//	                       tests strip the cancel tail and splice events-deactivate.json
//	run-rejection.json     a run containing a handler rejection at the cap
//	events-deactivate.json    a soft-deactivate UpdateAccepted/Completed pair
//
// Recaptured with:
//
//	make enroll ID=hist && ...adds...
//	docker compose --env-file .env -f deploy/docker-compose.yml exec -T \
//	  -e TEMPORAL_ADDRESS=temporal:7233 -e TEMPORAL_NAMESPACE=rewards \
//	  temporal temporal workflow show --workflow-id customer-hist \
//	  --run-id <run> -o json
//
// Testing against recorded server output rather than hand-built protos is
// deliberate: a synthetic history only ever proves the code agrees with whoever
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

// softDeactivatedRun is the historical cancel-ended fixture with its cancel
// tail removed and a soft-deactivate Update spliced on — product leave is an
// Update now, not CancelWorkflow.
func softDeactivatedRun(t *testing.T) []*historypb.HistoryEvent {
	t.Helper()
	events := loadEvents(t, "run-deactivated.json")
	cut := len(events)
	for i, e := range events {
		if e.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCEL_REQUESTED {
			cut = i
			break
		}
	}
	return append(events[:cut], loadEvents(t, "events-deactivate.json")...)
}

// membershipUpdate builds the Accepted/Completed pair Temporal writes for a
// deactivate or reactivate Update.
//
// Built rather than captured, unlike every fixture above, because the cases that
// matter are the *combinations* -- leave, rejoin, repeat leave, no-op rejoin.
// TestMembershipUpdateMatchesTheCapturedPair keeps the builder honest against
// the one pair recorded from a real server.
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

func kinds(entries []AuditEntry) []AuditEntryKind {
	out := make([]AuditEntryKind, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

func requireKinds(t *testing.T, got []AuditEntry, want ...AuditEntryKind) {
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
func TestAuditRun_EnrollmentRun(t *testing.T) {
	run := auditRun("run-0", loadEvents(t, "run-enrollment.json"))

	if run.previousRunID != "" {
		t.Errorf("previousRunID = %q, want empty on the enrollment run", run.previousRunID)
	}
	if run.startState.Generation != 0 {
		t.Errorf("generation = %d, want 0", run.startState.Generation)
	}
	if run.startState.LifetimeEarnEvents != 0 {
		t.Errorf("lifetimeEarnEvents at enrollment = %d, want 0", run.startState.LifetimeEarnEvents)
	}
	requireKinds(t, run.entries,
		AuditEnrolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded)
	if run.earnEvents != 3 {
		t.Errorf("earnEvents = %d, want 3 (EarnsPerRun)", run.earnEvents)
	}

	// The request half of the row comes from the accepted event, the outcome
	// half from the completed one.
	add := run.entries[1]
	if add.Amount != 1000 || add.Reason == "" {
		t.Errorf("request side not decoded: amount=%d reason=%q", add.Amount, add.Reason)
	}
	if add.Balance != 1000 || add.Level != rewards.LevelPlatinum {
		t.Errorf("outcome side not decoded: balance=%d level=%q", add.Balance, add.Level)
	}
	if add.RequestID == "" {
		t.Error("RequestID should carry the Update ID, which is the caller's idempotency key")
	}
	if add.At.IsZero() || add.EventID == 0 {
		t.Errorf("row not anchored: at=%v eventId=%d", add.At, add.EventID)
	}

	// Balances are cumulative down the run.
	for i, want := range []int{1000, 2000, 3000} {
		if got := run.entries[i+1].Balance; got != want {
			t.Errorf("entry %d balance = %d, want %d", i+1, got, want)
		}
	}
}

// A run that was rolled into. The generation divider is recorded here rather
// than on the predecessor's ContinuedAsNew event, so it survives the
// predecessor being reaped.
func TestAuditRun_ContinuedRun(t *testing.T) {
	run := auditRun("run-1", loadEvents(t, "run-continued.json"))

	if run.previousRunID == "" {
		t.Fatal("previousRunID should name the predecessor run")
	}
	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded)

	if run.entries[0].Generation != 1 {
		t.Errorf("divider generation = %d, want 1 (the generation being entered)",
			run.entries[0].Generation)
	}
	if run.entries[0].RunID != "run-1" {
		t.Errorf("divider runId = %q, want the successor's", run.entries[0].RunID)
	}
	// Carried state: this run has no idea what happened in the previous one
	// beyond these numbers.
	if run.startState.LifetimeEarnEvents != 3 {
		t.Errorf("carried lifetimeEarnEvents = %d, want 3", run.startState.LifetimeEarnEvents)
	}
	if run.startState.Points != 3000 {
		t.Errorf("carried points = %d, want 3000", run.startState.Points)
	}
	// Every row in a run is tagged with that run's generation, so the UI can
	// group without tracking the dividers itself.
	for i, e := range run.entries {
		if e.Generation != 1 {
			t.Errorf("entry %d generation = %d, want 1", i, e.Generation)
		}
	}
}

// Soft-deactivate is an Update, so it appears as Accepted/Completed rather than
// a CancelRequested event.
func TestAuditRun_DeactivatedRun(t *testing.T) {
	run := auditRun("run-2", softDeactivatedRun(t))

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated)
	if run.entries[0].Generation != 2 {
		t.Errorf("generation = %d, want 2", run.entries[0].Generation)
	}
	if run.entries[2].At.IsZero() {
		t.Error("deactivation row needs a timestamp -- it is the last thing the page shows")
	}
}

// The builder used by the tests below has to produce what the server actually
// wrote, or those tests only prove the code agrees with the builder. Checked
// against the one membership pair that was captured from a real run.
func TestMembershipUpdateMatchesTheCapturedPair(t *testing.T) {
	captured := loadEvents(t, "events-deactivate.json")
	built := membershipUpdate(t, captured[0].GetEventId(),
		rewards.UpdateDeactivate, "soft-deactivate-1", rewards.DeactivateResult{Changed: true})

	gotK := kinds(auditRun("run-x", built).entries)
	wantK := kinds(auditRun("run-x", captured).entries)
	if len(gotK) != len(wantK) || (len(wantK) == 1 && gotK[0] != wantK[0]) {
		t.Fatalf("built pair renders as %v, captured pair renders as %v", gotK, wantK)
	}

	g, w := auditRun("run-x", built).entries[0], auditRun("run-x", captured).entries[0]
	if g.RequestID != w.RequestID || g.EventID != w.EventID {
		t.Errorf("built {%s, %d}, captured {%s, %d}", g.RequestID, g.EventID, w.RequestID, w.EventID)
	}
}

// Without a rejoin row, a customer who left and came back reads as permanently
// departed with unexplained point-adds after the departure.
func TestAuditRun_ReactivationDrawsARow(t *testing.T) {
	events := append(softDeactivatedRun(t),
		membershipUpdate(t, 200, rewards.UpdateReactivate, "rejoin-1",
			rewards.ReactivateResult{Changed: true})...)

	run := auditRun("run-2", events)

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated, AuditReactivated)
	if got := run.entries[3].RequestID; got != "rejoin-1" {
		t.Errorf("requestId = %q, want the update ID that asked for it", got)
	}
	// Rejoining is not earning. Counting it would inflate "showing N of M".
	if run.earnEvents != 1 {
		t.Errorf("earnEvents = %d, want 1 -- a rejoin is not a point event", run.earnEvents)
	}
}

// Both membership Updates are idempotent, so both write history for calls that
// changed nothing: a repeat DELETE, a re-enroll of someone already active. Those
// completions are real events, but rendering them would show a customer leaving
// twice or rejoining a program they never left.
func TestAuditRun_IdempotentMembershipCallsDrawNoRow(t *testing.T) {
	events := softDeactivatedRun(t)
	events = append(events, membershipUpdate(t, 200, rewards.UpdateDeactivate, "repeat-delete",
		rewards.DeactivateResult{Changed: false})...)
	events = append(events, membershipUpdate(t, 300, rewards.UpdateReactivate, "rejoin-1",
		rewards.ReactivateResult{Changed: true})...)
	events = append(events, membershipUpdate(t, 400, rewards.UpdateReactivate, "duplicate-enroll",
		rewards.ReactivateResult{Changed: false})...)

	run := auditRun("run-2", events)

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated, AuditReactivated)
}

// The failure path. Both handlers stage their change and commit only once the
// search attribute upsert is issued, so a failed membership Update applied
// nothing -- and unlike a failed addPoints, there is no half-state to disclose.
func TestAuditRun_FailedMembershipUpdateDrawsNoRow(t *testing.T) {
	events := softDeactivatedRun(t)
	pair := membershipUpdate(t, 200, rewards.UpdateReactivate, "rejoin-failed",
		rewards.ReactivateResult{Changed: true})
	pair[1].GetWorkflowExecutionUpdateCompletedEventAttributes().Outcome = &updatepb.Outcome{
		Value: &updatepb.Outcome_Failure{
			Failure: &failurepb.Failure{Message: "upsert search attributes: boom"},
		},
	}

	run := auditRun("run-2", append(events, pair...))

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated)
}

// Neither membership Update may render as a point-add. They share the Update
// event types with addPoints, and the amount/reason fields decode to zero from
// their arguments, so a missed name check shows up as a silent "+0 ()" row
// rather than as a failure.
func TestAuditRun_MembershipUpdatesAreNeverPointRows(t *testing.T) {
	events := softDeactivatedRun(t)
	events = append(events, membershipUpdate(t, 200, rewards.UpdateReactivate, "rejoin-1",
		rewards.ReactivateResult{Changed: true})...)

	for _, e := range auditRun("run-2", events).entries {
		if e.Kind == AuditPointsAdded && e.Amount == 0 {
			t.Errorf("a membership Update rendered as a point-add: %+v", e)
		}
	}
}

// The recorded half of the validator/handler split. A handler rejection writes an
// Accepted and a Completed-with-failure, so it becomes a row; a validator
// rejection writes nothing at all and can never appear here.
func TestAuditRun_HandlerRejectionIsRecorded(t *testing.T) {
	run := auditRun("cap-run", loadEvents(t, "run-rejection.json"))

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditPointsRejected)

	rejected := run.entries[2]
	if rejected.Failure == "" {
		t.Error("rejection row must carry the workflow's own message")
	}
	if rejected.Balance != 0 || rejected.Level != "" {
		t.Errorf("a rejected add has no outcome: balance=%d level=%q",
			rejected.Balance, rejected.Level)
	}
	if rejected.Amount != 500 {
		t.Errorf("the attempted amount = %d, want 500 -- what was refused is the point of the row",
			rejected.Amount)
	}

	// A rejection is not an earn. Counting it would inflate ShownEarnEvents and
	// make an intact log look truncated against the carried lifetime count.
	if run.earnEvents != 1 {
		t.Errorf("earnEvents = %d, want 1 -- rejections must not count", run.earnEvents)
	}
}

// --- the walk ---------------------------------------------------------------

// fakeChain serves a synthetic run chain, and reaps everything older than
// `oldest` the way the server does -- by returning NotFound for a run whose
// successor still names it.
func fakeChain(runs map[string][]*historypb.HistoryEvent) historyFetcher {
	return func(_ context.Context, runID string) ([]*historypb.HistoryEvent, error) {
		events, ok := runs[runID]
		if !ok {
			return nil, serviceerror.NewNotFound("workflow execution not found")
		}
		return events, nil
	}
}

func TestWalkRuns_StopsAtEnrollment(t *testing.T) {
	enrollment := loadEvents(t, "run-enrollment.json")
	continued := loadEvents(t, "run-continued.json")
	// Make the fake chain answer to the predecessor the start event names,
	// rather than to one this test invented.
	prev := continued[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()

	runs, truncated, err := walkRuns(context.Background(),
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
	if runs[0].runID != "newest" || runs[1].runID != prev {
		t.Errorf("walk order = %q, %q; want newest first", runs[0].runID, runs[1].runID)
	}
}

// The truncation case, and the reason the audit endpoint has a Truncated field
// at all: the predecessor was reaped, so the walk cannot reach enrollment.
func TestWalkRuns_TruncatedWhenPredecessorReaped(t *testing.T) {
	continued := loadEvents(t, "run-continued.json")

	runs, truncated, err := walkRuns(context.Background(),
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

// A NotFound on the very first run is not truncation. Describe just resolved it,
// so it disappearing is a real fault and must not be served as a short timeline.
func TestWalkRuns_FirstRunNotFoundIsAnError(t *testing.T) {
	_, _, err := walkRuns(context.Background(),
		fakeChain(map[string][]*historypb.HistoryEvent{}), "gone")

	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want a NotFound to propagate", err)
	}
}

// --- assembly ---------------------------------------------------------------

func TestAssemble_NewestFirst(t *testing.T) {
	runs := []runAudit{ // newest first, as the walk produces them
		{runID: "b", entries: []AuditEntry{
			{Kind: AuditGenerationRolled, RunID: "b"},
			{Kind: AuditPointsAdded, RunID: "b"},
		}, earnEvents: 1,
			startState: rewards.CustomerState{LifetimeEarnEvents: 2}},
		{runID: "a", entries: []AuditEntry{{Kind: AuditEnrolled, RunID: "a"}}, earnEvents: 2},
	}

	got := assemble("ada", runs, false)
	// Newest run first, and within it the newest event first.
	requireKinds(t, got.Entries, AuditPointsAdded, AuditGenerationRolled, AuditEnrolled)
	if got.Entries[0].RunID != "b" || got.Entries[2].RunID != "a" {
		t.Errorf("entries not ordered newest-first: %v", got.Entries)
	}
	if got.RunsWalked != 2 {
		t.Errorf("runsWalked = %d, want 2", got.RunsWalked)
	}
	if got.OldestRunID != "" {
		t.Errorf("oldestRunId = %q, want empty when the crawl reached enrollment", got.OldestRunID)
	}
}

// ShownEarnEvents equals LifetimeEarnEvents whenever the log is complete,
// because the carried count at the newest run's start is exactly the sum of the
// earns in every run before it.
func TestAssemble_ShownEqualsLifetimeWhenComplete(t *testing.T) {
	runs := []runAudit{
		{runID: "c", earnEvents: 1, startState: rewards.CustomerState{LifetimeEarnEvents: 6}},
		{runID: "b", earnEvents: 3, startState: rewards.CustomerState{LifetimeEarnEvents: 3}},
		{runID: "a", earnEvents: 3},
	}

	got := assemble("ada", runs, false)
	if got.ShownEarnEvents != 7 || got.LifetimeEarnEvents != 7 {
		t.Errorf("shown=%d lifetime=%d, want both 7", got.ShownEarnEvents, got.LifetimeEarnEvents)
	}
}

// And the case it exists for: history reaped, so the log is short but the count
// is still right -- "Showing 3 of 21 point events."
func TestAssemble_LifetimeSurvivesTruncation(t *testing.T) {
	runs := []runAudit{
		{runID: "gen7", earnEvents: 2, startState: rewards.CustomerState{LifetimeEarnEvents: 19}},
		{runID: "gen6", earnEvents: 1, startState: rewards.CustomerState{LifetimeEarnEvents: 18}},
	}

	got := assemble("grace", runs, true)
	if got.ShownEarnEvents != 3 {
		t.Errorf("shown = %d, want 3", got.ShownEarnEvents)
	}
	if got.LifetimeEarnEvents != 21 {
		t.Errorf("lifetime = %d, want 21 -- it comes from the carried state, not the rows",
			got.LifetimeEarnEvents)
	}
	if !got.Truncated {
		t.Error("truncated flag lost")
	}
	if got.OldestRunID != "gen6" {
		t.Errorf("oldestRunId = %q, want the oldest run actually read", got.OldestRunID)
	}
}

// A customer whose whole execution is gone produces no runs. Entries must still
// be an empty array rather than JSON null, which a UI mapping over it would
// crash on.
func TestAssemble_EmptyIsNotNull(t *testing.T) {
	got := assemble("ghost", nil, false)
	if got.Entries == nil {
		t.Fatal("Entries must serialise as [] rather than null")
	}
	if got.RunsWalked != 0 || got.LifetimeEarnEvents != 0 {
		t.Errorf("unexpected counts: %+v", got)
	}
}

// --- shape of the whole thing -----------------------------------------------

// End to end over the golden chain, in the order the walk produces: the three
// runs of one customer's life, rendered as one timeline.
func TestCrawlShape_WholeCustomerLife(t *testing.T) {
	deactivated := softDeactivatedRun(t)
	continued := loadEvents(t, "run-continued.json")
	enrollment := loadEvents(t, "run-enrollment.json")

	// Predecessor run IDs still come from the historical cancel fixture's start
	// event (the soft-deactivate splice does not replace it).
	raw := loadEvents(t, "run-deactivated.json")
	gen1 := raw[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()
	gen0 := continued[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()

	runs, truncated, err := walkRuns(context.Background(), fakeChain(map[string][]*historypb.HistoryEvent{
		"gen2": deactivated, gen1: continued, gen0: enrollment,
	}), "gen2")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	got := assemble("hist", runs, truncated)
	requireKinds(t, got.Entries,
		AuditDeactivated, AuditPointsAdded, AuditGenerationRolled,
		AuditPointsAdded, AuditPointsAdded, AuditPointsAdded, AuditGenerationRolled,
		AuditPointsAdded, AuditPointsAdded, AuditPointsAdded, AuditEnrolled)

	if got.ShownEarnEvents != 7 || got.LifetimeEarnEvents != 7 {
		t.Errorf("shown=%d lifetime=%d, want both 7", got.ShownEarnEvents, got.LifetimeEarnEvents)
	}
	if got.Truncated {
		t.Error("an intact chain must not report truncation")
	}

	// Non-increasing in time, which is what "newest first" has to mean for a
	// timeline assembled from separately-fetched runs.
	for i := 1; i < len(got.Entries); i++ {
		if got.Entries[i].At.After(got.Entries[i-1].At) {
			t.Errorf("entry %d (%s) at %v jumps forward from %v",
				i, got.Entries[i].Kind, got.Entries[i].At, got.Entries[i-1].At)
		}
	}
}
