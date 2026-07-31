package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"google.golang.org/protobuf/encoding/protojson"
)

// The golden files in testdata/ are verbatim `temporal workflow show -o json`
// output from the local stack, one file per run:
//
//	run-enrollment.json    the first run: enrollment + 3 adds, then the roll
//	run-continued.json     a middle run: rolled into, 3 adds, rolled out of
//	run-deactivated.json   the last run: rolled into, 1 add, then cancelled
//	run-rejection.json     a run containing a handler rejection at the cap
//	events-notification.json  a real NotifyCustomer Activity pair (see below)
//
// Recaptured with:
//
//	make enroll ID=hist && ...adds...
//	docker compose --env-file .env -f deploy/docker-compose.yml exec -T \
//	  -e TEMPORAL_ADDRESS=temporal:7233 -e TEMPORAL_NAMESPACE=rewards \
//	  temporal temporal workflow show --workflow-id customer-hist \
//	  --run-id <run> -o json
//
// Testing the mapping against recorded server output rather than against
// hand-built protos is deliberate. Every phase of this project has found the
// plan wrong about some platform detail, and a synthetic history only ever
// proves the code agrees with whoever wrote the test. These bytes are the ones
// Temporal actually wrote.

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
	// half from the completed one -- the pairing PLAN.md 6.2 describes.
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

	// Balances are cumulative down the run, which is what makes the timeline
	// readable without the reader doing arithmetic.
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
	// Carried state, which is the whole point of continue-as-new: this run has
	// no idea what happened in the previous one beyond these numbers.
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

// Deactivation is a cancellation, so it appears as a request event rather than
// as anything the workflow chose to record. PLAN.md 3.6.
func TestAuditRun_DeactivatedRun(t *testing.T) {
	run := auditRun("run-2", loadEvents(t, "run-deactivated.json"))

	requireKinds(t, run.entries,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated)
	if run.entries[0].Generation != 2 {
		t.Errorf("generation = %d, want 2", run.entries[0].Generation)
	}
	if run.entries[2].At.IsZero() {
		t.Error("deactivation row needs a timestamp -- it is the last thing the page shows")
	}
}

// The recorded half of the validator/handler split. A handler rejection writes
// an Accepted and a Completed-with-failure, so it becomes a row; a validator
// rejection writes nothing at all and can never appear here. PLAN.md 3.4.
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

// The notification rows PLAN.md 3.7 says the crawl picks up "for free".
//
// The Activity events here are real, captured from a throwaway workflow that
// scheduled an activity named NotifyCustomer with a rewards.NotifyRequest -- the
// signature 3.7 specifies -- because Phase 6 has not been written yet. Splicing
// them into a real enrollment run tests the mapping against server bytes without
// waiting for the Activity itself. Phase 6 should delete this splice and assert
// against its own history.
func TestAuditRun_NotificationRows(t *testing.T) {
	events := append(loadEvents(t, "run-enrollment.json"), loadEvents(t, "events-notification.json")...)
	run := auditRun("run-0", events)

	requireKinds(t, run.entries,
		AuditEnrolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded, AuditNotificationSent)

	note := run.entries[4]
	if note.NotifiedLevel != rewards.LevelGold {
		t.Errorf("notifiedLevel = %q, want %q", note.NotifiedLevel, rewards.LevelGold)
	}
	if note.At.IsZero() {
		t.Error("notification row needs a timestamp")
	}
	// A notification is not an earn either.
	if run.earnEvents != 3 {
		t.Errorf("earnEvents = %d, want 3", run.earnEvents)
	}
}

// Emitted on completion, not on scheduling: "sent" has to mean sent. An
// Activity that was scheduled and never completed leaves no row.
func TestAuditRun_UncompletedNotificationIsNotReported(t *testing.T) {
	notify := loadEvents(t, "events-notification.json")
	scheduledOnly := notify[:1]
	if scheduledOnly[0].GetActivityTaskScheduledEventAttributes() == nil {
		t.Fatal("expected the first captured event to be the scheduling")
	}

	run := auditRun("run-0", append(loadEvents(t, "run-enrollment.json"), scheduledOnly...))
	for _, e := range run.entries {
		if e.Kind == AuditNotificationSent {
			t.Fatal("a scheduled-but-never-completed notification must not render as sent")
		}
	}
}

// Only NotifyCustomer becomes a notification row. Nothing else schedules an
// Activity today, but the crawl is the one component that sees everything a
// workflow ever did -- so the day a later phase adds an unrelated Activity, it
// must not start announcing itself to customers as a notification.
func TestAuditRun_OtherActivitiesAreIgnored(t *testing.T) {
	notify := loadEvents(t, "events-notification.json")
	notify[0].GetActivityTaskScheduledEventAttributes().
		GetActivityType().Name = "SomeOtherActivity"

	run := auditRun("run-0", append(loadEvents(t, "run-enrollment.json"), notify...))
	requireKinds(t, run.entries,
		AuditEnrolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded)
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
	// The continued run's start event names its predecessor; make the fake chain
	// answer to that name rather than to one this test invented.
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

func TestAssemble_OldestFirst(t *testing.T) {
	runs := []runAudit{ // newest first, as the walk produces them
		{runID: "b", entries: []AuditEntry{{Kind: AuditGenerationRolled, RunID: "b"}}, earnEvents: 1,
			startState: rewards.CustomerState{LifetimeEarnEvents: 2}},
		{runID: "a", entries: []AuditEntry{{Kind: AuditEnrolled, RunID: "a"}}, earnEvents: 2},
	}

	got := assemble("ada", runs, false)
	if got.Entries[0].RunID != "a" || got.Entries[1].RunID != "b" {
		t.Errorf("entries not reversed to oldest-first: %v", got.Entries)
	}
	if got.RunsWalked != 2 {
		t.Errorf("runsWalked = %d, want 2", got.RunsWalked)
	}
	if got.OldestRunID != "" {
		t.Errorf("oldestRunId = %q, want empty when the crawl reached enrollment", got.OldestRunID)
	}
}

// The contract's own claim: ShownEarnEvents equals LifetimeEarnEvents whenever
// the log is complete. It holds because the carried count at the newest run's
// start is exactly the sum of the earns in every run before it.
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
	deactivated := loadEvents(t, "run-deactivated.json")
	continued := loadEvents(t, "run-continued.json")
	enrollment := loadEvents(t, "run-enrollment.json")

	gen1 := deactivated[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()
	gen0 := continued[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()

	runs, truncated, err := walkRuns(context.Background(), fakeChain(map[string][]*historypb.HistoryEvent{
		"gen2": deactivated, gen1: continued, gen0: enrollment,
	}), "gen2")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	got := assemble("hist", runs, truncated)
	requireKinds(t, got.Entries,
		AuditEnrolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded,
		AuditGenerationRolled, AuditPointsAdded, AuditPointsAdded, AuditPointsAdded,
		AuditGenerationRolled, AuditPointsAdded, AuditDeactivated)

	if got.ShownEarnEvents != 7 || got.LifetimeEarnEvents != 7 {
		t.Errorf("shown=%d lifetime=%d, want both 7", got.ShownEarnEvents, got.LifetimeEarnEvents)
	}
	if got.Truncated {
		t.Error("an intact chain must not report truncation")
	}

	// Non-decreasing in time, which is what "oldest first" has to mean for a
	// timeline assembled from separately-fetched runs.
	var prev time.Time
	for i, e := range got.Entries {
		if e.At.Before(prev) {
			t.Errorf("entry %d (%s) at %v goes back in time from %v", i, e.Kind, e.At, prev)
		}
		prev = e.At
	}
}
