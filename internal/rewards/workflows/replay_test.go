package workflows_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/internalbindings"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Replay test.
//
// A customer's workflow *will* outlive a deploy. A worker picking up a task for
// a run started weeks ago replays that run's recorded history through today's
// code and requires the commands to match event for event; if they do not, the
// workflow task fails and retries forever and the customer is silently wedged.
// Replaying a recorded history here is the rehearsal of that deploy.

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
// Without it the replayer invents the workflow ID "ReplayId", ValidateEnrollment
// refuses a payload whose customerId does not match, and the workflow returns
// before emitting a single command -- which surfaces as a nondeterminism error
// naming whatever event came first. If this test starts failing that way after a
// "simplification" of this helper, this is why.
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

// A real history recorded by this workflow: adds that cross a tier and the
// notification Activity that follows. Today's code must reproduce its commands
// exactly; an edit that changes what the workflow emits fails here before it
// wedges an open run in production.
func TestReplay_RecordedHistory(t *testing.T) {
	h := loadHistory(t, "run-notification.json")

	if err := replay(t, h); err != nil {
		t.Fatalf("a history this workflow produced does not replay through it: %v", err)
	}

	// Sanity-check the fixture is the interesting one rather than a bare run:
	// it must schedule the notification Activity, or the replay above proves
	// much less than it looks.
	var sawActivity bool
	for _, e := range h.GetEvents() {
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil &&
			a.GetActivityType().GetName() == rewards.ActivityNotifyCustomer {
			sawActivity = true
		}
	}
	if !sawActivity {
		t.Error("fixture schedules no NotifyCustomer activity; recapture it from a run that crosses a tier")
	}
}
