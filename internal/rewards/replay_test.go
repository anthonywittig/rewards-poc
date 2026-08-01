package rewards_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

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
	r.RegisterWorkflow(rewards.CustomerRewardsWorkflow)

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
	r.RegisterWorkflow(rewards.CustomerRewardsWorkflow)

	err := r.ReplayWorkflowHistory(nil, loadHistory(t, "pre-notification-enrollment.json"))
	if err == nil {
		t.Fatal("the plain replay call now works -- drop the OriginalExecution workaround in replay()")
	}
	if !strings.Contains(err.Error(), "nondeterministic") {
		t.Errorf("unexpected failure shape, worth re-reading: %v", err)
	}
}
