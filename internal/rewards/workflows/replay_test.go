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

// Replay tests. FINDINGS.md#versioning-is-the-real-risk.
//
// A customer's workflow *will* outlive a deploy. A worker picking up a task for
// a run started weeks ago replays that run's recorded history through today's
// code and requires the commands to match event for event; if they do not, the
// workflow task fails and retries forever and the customer is silently wedged.
//
// testdata/pre-notification-* were recorded before the notification Activity
// existed, so replaying them rehearses the deploy that adds it.

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
// notification Activity existed; all of them must still replay. Remove the
// GetVersion gate in CustomerRewardsWorkflow and these four fail with:
//
//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
//	  (ActivityType:(Name:NotifyCustomer) ...)
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

// The trap, pinned. worker.ReplayWorkflowHistory documents OriginalExecution as
// "optional", but for any workflow that reads its own ID it is not: the replayer
// substitutes "ReplayId", the enrollment check rejects the payload, and the run
// emits no commands at all.
//
// The failure is doubly misleading -- it reports nondeterminism, so it looks
// like a versioning problem, and it names whichever event came first, so it
// points at innocent code.
//
// Asserted rather than merely commented, so nobody "simplifies" replay() back to
// the plain call and gets a suite that passes while testing nothing.
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
// testdata/ungated-notification.json is real -- an execution whose history
// contains a NotifyCustomer Activity and *no* Version marker. Replaying it
// through the gated workflow fails, because GetVersion cannot tell "predates the
// change" from "ran the change before it was gated":
//
//	lookup failed for scheduledEventID to activityID: scheduleEventID: 24
//
// There is no code fix -- whichever way DefaultVersion is interpreted, one of
// the two populations breaks. Gating protects every run recorded before the
// change, at the cost of those recorded in the window between two commits.
//
// The remedy is operational, and the affected executions are findable because
// GetVersion upserts TemporalChangeVersion
// (FINDINGS.md#getversion-writes-two-events):
//
//	WorkflowType = 'CustomerRewardsWorkflow'
//	  AND ExecutionStatus = 'Running'
//	  AND TemporalChangeVersion IS NULL
//	  AND StartTime > '<when the ungated build was deployed>'
//
// ...then `make reset`, or a targeted terminate. The StartTime clause separates
// them from older runs, which also have no marker but replay perfectly well.
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
			"update this test and FINDINGS.md#versioning-is-the-real-risk")
	}
}

// The departure half of the gate.
//
// A customer enrolled before the marker can still be deactivated today: their
// run is live, it has no marker, and the deactivate Update arrives as new
// history on top of it. The gate has to keep that Update from arming the
// departure notice, or the Activity goes into history that a later replay --
// still resolving to DefaultVersion -- would decline to produce, wedging the
// customer at the very moment they leave.
//
// testdata/pre-marker-deactivated.json is that run: no marker, no Activities, an
// addPoints that crosses gold, and a deactivate. Recorded by running the current
// workflow with the GetVersion call replaced by a bare `false`; the replayer
// sees recorded events, not how they were made.
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
