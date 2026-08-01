package workflows_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/internalbindings"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Replay tests: the highest-value tests in this repo, and the ones PLAN.md 10
// calls the single biggest operational risk in the design.
//
// A customer's workflow runs forever. It *will* outlive a deploy. When a worker
// picks up a task for a run that started weeks ago, it replays that run's
// recorded history through today's code and requires the commands to match
// event for event. If they do not, the workflow task fails and retries forever:
// the customer is wedged, silently, and no amount of restarting helps.
//
// So these histories are checked in deliberately. testdata/pre-notification-*
// were recorded by the *Phase 5* worker, before the notification Activity
// existed, and replaying them is a rehearsal of the deploy that adds it.

// replayCase is one recorded run.
type replayCase struct {
	file string
	what string
}

var preNotificationRuns = []replayCase{
	{"pre-notification-enrollment.json", "first run: enrollment, three adds, then the roll"},
	{"pre-notification-continued.json", "middle run: rolled into, three adds, rolled out of"},
	{"pre-notification-deactivated.json", "final run: one add, then cancelled"},
	{"pre-notification-rejection.json", "a run carrying a handler rejection at the cap"},
}

func loadHistory(t *testing.T, name string) *historypb.History {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var h historypb.History
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &h); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return &h
}

// replay runs one history through the current workflow code.
//
// OriginalExecution is not optional here, whatever the SDK's doc comment says.
// Without it the replayer invents the workflow ID "ReplayId", validateEnrollment
// refuses a payload whose customerId does not match, and the workflow returns
// before emitting a single command -- which surfaces as a nondeterminism error
// naming whatever event came first. See TestReplay_NeedsTheRecordedWorkflowID.
func replay(t *testing.T, h *historypb.History) error {
	t.Helper()
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflow(workflows.CustomerRewardsWorkflow)

	started := h.GetEvents()[0].GetWorkflowExecutionStartedEventAttributes()
	if started == nil {
		t.Fatal("history does not begin with WorkflowExecutionStarted")
	}
	return r.ReplayWorkflowHistoryWithOptions(nil, h, worker.ReplayWorkflowHistoryOptions{
		OriginalExecution: internalbindings.WorkflowExecution{ID: started.GetWorkflowId()},
	})
}

// The deploy rehearsal. Every one of these histories was recorded before the
// notification Activity existed; all of them must still replay.
//
// This is the test that caught the real thing. Phase 6 emitted a
// ScheduleActivityTask command that these histories have no event for:
//
//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
//	  (ActivityType:(Name:NotifyCustomer) ...)
//
// Deploying that as written would have wedged every customer with an open run.
// The fix is the GetVersion gate in CustomerRewardsWorkflow; remove it and these
// four fail again.
func TestReplay_HistoriesRecordedBeforeTheNotificationActivity(t *testing.T) {
	for _, tc := range preNotificationRuns {
		t.Run(tc.file, func(t *testing.T) {
			if err := replay(t, loadHistory(t, tc.file)); err != nil {
				t.Errorf("%s (%s) no longer replays: %v", tc.file, tc.what, err)
			}
		})
	}
}

// A history recorded by the *current* code, version marker and all. Guards the
// other direction: that today's workflow is self-consistent, and that the marker
// and its automatic TemporalChangeVersion upsert replay cleanly.
func TestReplay_HistoryRecordedByTheCurrentWorkflow(t *testing.T) {
	h := loadHistory(t, "run-versioned-notification.json")

	if err := replay(t, h); err != nil {
		t.Fatalf("a history this code produced does not replay through it: %v", err)
	}

	// Sanity-check the fixture is the interesting one rather than a bare run:
	// it must carry both the version marker and a notification Activity, or the
	// test above is replaying something that proves much less than it looks.
	var sawMarker, sawActivity bool
	for _, e := range h.GetEvents() {
		if m := e.GetMarkerRecordedEventAttributes(); m != nil && m.GetMarkerName() == "Version" {
			sawMarker = true
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil &&
			a.GetActivityType().GetName() == rewards.ActivityNotifyCustomer {
			sawActivity = true
		}
	}
	if !sawMarker {
		t.Error("fixture has no Version marker; recapture it from a post-Phase-6 worker")
	}
	if !sawActivity {
		t.Error("fixture schedules no NotifyCustomer activity; recapture it from a run that crosses a tier")
	}
}

// The trap, pinned.
//
// worker.ReplayWorkflowHistory documents OriginalExecution as "optional", and
// for most workflows it is. For any workflow that reads its own ID it is not:
// the replayer substitutes "ReplayId", so CustomerRewardsWorkflow's enrollment
// check (which exists because the workflow is the only integrity boundary --
// PLAN.md 3.1) rejects the payload and the run emits no commands at all.
//
// The failure is doubly misleading. It reports nondeterminism, so it looks like
// a versioning problem; and it names whichever event happened to come first
// (here, UpsertWorkflowSearchAttributes), so it points at code that is entirely
// innocent. A whole afternoon is available to anyone who trusts it.
//
// Asserted rather than merely commented, so that nobody "simplifies" replay()
// back to the plain call and gets a suite that passes while testing nothing --
// and so that if a future SDK starts honouring the recorded ID, this fails and
// tells us the workaround can go.
func TestReplay_NeedsTheRecordedWorkflowID(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflow(workflows.CustomerRewardsWorkflow)

	err := r.ReplayWorkflowHistory(nil, loadHistory(t, "pre-notification-enrollment.json"))
	if err == nil {
		t.Fatal("the plain replay call now works -- drop the OriginalExecution workaround in replay()")
	}
	if !strings.Contains(err.Error(), "nondeterministic") {
		t.Errorf("unexpected failure shape, worth re-reading: %v", err)
	}
}

// A limitation, asserted so it cannot be forgotten: the gate arrived one commit
// too late to save the runs the ungated code created.
//
// testdata/ungated-notification.json is real, recorded by the Phase 6 code
// exactly as it merged -- an execution whose history contains a NotifyCustomer
// Activity and *no* Version marker. Replaying it through the gated workflow
// fails, because GetVersion cannot tell "this run predates the change" from
// "this run executed the change before it was gated". Both lack the marker, and
// the marker is the only signal there is:
//
//	lookup failed for scheduledEventID to activityID: scheduleEventID: 24
//
// There is no code fix. Whichever way DefaultVersion is interpreted, one of the
// two populations breaks -- gating it (as we do) protects every run recorded
// before Phase 6, at the cost of every run recorded by Phase 6 itself. That is
// the right trade, because the first population is "all customers from before
// the deploy" and the second is only those started inside the window between
// two commits.
//
// The remedy is operational, and the affected executions are findable, because
// GetVersion upserts TemporalChangeVersion (PLAN.md 12.36):
//
//	WorkflowType = 'CustomerRewardsWorkflow'
//	  AND ExecutionStatus = 'Running'
//	  AND TemporalChangeVersion IS NULL
//	  AND StartTime > '<when the ungated build was deployed>'
//
// ...then `make reset`, or a targeted terminate. The StartTime clause is what
// separates them from pre-Phase-6 runs, which also have no marker and replay
// perfectly well.
//
// The real lesson is upstream of all of this: gate a command-changing edit in
// the *same* commit that introduces it. Phase 6 did not, and no amount of
// cleverness in Phase 9 can reach back and fix the histories it wrote.
func TestReplay_UngatedPhase6HistoriesCannotBeRescued(t *testing.T) {
	h := loadHistory(t, "ungated-notification.json")

	// Confirm the fixture is the thing this test claims: an Activity, no marker.
	var sawMarker, sawActivity bool
	for _, e := range h.GetEvents() {
		if m := e.GetMarkerRecordedEventAttributes(); m != nil && m.GetMarkerName() == "Version" {
			sawMarker = true
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil &&
			a.GetActivityType().GetName() == rewards.ActivityNotifyCustomer {
			sawActivity = true
		}
	}
	if sawMarker || !sawActivity {
		t.Fatalf("fixture is not an ungated Phase 6 history (marker=%v activity=%v)",
			sawMarker, sawActivity)
	}

	if err := replay(t, h); err == nil {
		t.Fatal("these histories now replay -- a way to rescue them has been found; " +
			"update this test and PLAN.md 12.11")
	}
}

// The departure half of the gate, which nothing covered until soft
// deactivation made it reachable.
//
// A customer enrolled before Phase 6 can still be deactivated today: their run
// is live, it has no Version marker, and the deactivate Update arrives as new
// history on top of it. The gate has to keep that Update from arming the
// departure notice, because the Activity would go into history that a later
// replay -- still resolving to DefaultVersion, still finding no marker -- would
// decline to produce. That is a wedge, appended to a customer at the very moment
// they leave.
//
// testdata/pre-marker-deactivated.json is that run: no marker, no Activities,
// an addPoints that crosses gold, and a deactivate. Recorded by running the
// current workflow with the GetVersion call replaced by a bare `false`, which
// produces exactly the events a pre-Phase-6 customer would have -- the replayer
// sees recorded events, not how they were made.
//
// Until Phase 6's cancellation path was removed this was covered incidentally,
// because pre-notification-deactivated.json ends in a cancel and the departure
// notice hung off it. Soft deactivation moved the departure onto an Update and
// took the coverage with it -- caught by mutation, not by noticing.
func TestReplay_PreMarkerRunCanStillBeDeactivated(t *testing.T) {
	h := loadHistory(t, "pre-marker-deactivated.json")

	// The fixture only means something if it really is marker-free and
	// Activity-free; otherwise this passes for the wrong reason.
	for _, e := range h.GetEvents() {
		if m := e.GetMarkerRecordedEventAttributes(); m != nil && m.GetMarkerName() == "Version" {
			t.Fatal("fixture has a Version marker; recapture it without the GetVersion call")
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil {
			t.Fatalf("fixture already schedules %s; recapture it with notifications off",
				a.GetActivityType().GetName())
		}
	}
	var sawDeactivate bool
	for _, e := range h.GetEvents() {
		if u := e.GetWorkflowExecutionUpdateAcceptedEventAttributes(); u != nil &&
			u.GetAcceptedRequest().GetInput().GetName() == rewards.UpdateDeactivate {
			sawDeactivate = true
		}
	}
	if !sawDeactivate {
		t.Fatal("fixture carries no deactivate Update, so it cannot test the departure gate")
	}

	if err := replay(t, h); err != nil {
		t.Errorf("a pre-marker customer cannot be deactivated without wedging: %v", err)
	}
}
