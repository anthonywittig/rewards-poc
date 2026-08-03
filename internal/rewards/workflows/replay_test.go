package workflows_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/internalbindings"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Replay test: the deploy rehearsal.
//
// A customer's workflow *will* outlive a deploy. A worker picking up a task
// for a run started weeks ago replays that run's recorded history through
// today's code and requires the commands to match event for event; if they do
// not, the workflow task fails forever and the customer is silently wedged.

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
// OriginalExecution is not optional here: without it the replayer invents the
// workflow ID "ReplayId", ValidateEnrollment refuses the payload, and the
// workflow returns before emitting a single command -- surfacing as a
// nondeterminism error naming whatever event came first.
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

// A real recorded history: enrollment, three adds, and the roll into the next
// run. Today's code must reproduce its commands exactly; an edit that changes
// what the workflow emits fails here before it wedges an open run in production.
func TestReplay_RecordedHistory(t *testing.T) {
	h := loadHistory(t, "run-enrollment.json")

	if err := replay(t, h); err != nil {
		t.Fatalf("a history this workflow produced does not replay through it: %v", err)
	}

	// Sanity-check the fixture is the interesting one rather than a bare run:
	// it must actually roll over, or the replay above proves much less than it
	// looks.
	var sawRoll bool
	for _, e := range h.GetEvents() {
		if e.GetWorkflowExecutionContinuedAsNewEventAttributes() != nil {
			sawRoll = true
		}
	}
	if !sawRoll {
		t.Error("fixture never continues as new; recapture it from a run with EarnsPerRun adds")
	}
}
