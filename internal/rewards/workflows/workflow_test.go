package workflows

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// newEnv builds a test environment whose workflow ID matches the state's
// customer ID, since enrollment validation refuses a mismatch.
func newEnv(state rewards.CustomerState) *testsuite.TestWorkflowEnvironment {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID: rewards.WorkflowID(state.CustomerID),
	})
	return env
}

func enrolledState() rewards.CustomerState {
	return rewards.CustomerState{
		CustomerID: "ada-lovelace",
		Name:       "Ada Lovelace",
		Active:     true,
		RunNumber:  1,
	}
}

func statusOf(t *testing.T, env *testsuite.TestWorkflowEnvironment) rewards.CustomerStatus {
	t.Helper()
	enc, err := env.QueryWorkflow(rewards.QueryGetStatus)
	if err != nil {
		t.Fatalf("query %s: %v", rewards.QueryGetStatus, err)
	}
	var status rewards.CustomerStatus
	if err := enc.Get(&status); err != nil {
		t.Fatalf("decode %s result: %v", rewards.QueryGetStatus, err)
	}
	return status
}

// The hole this closes: a retry that straddles a continue-as-new arrives at a
// fresh run, past the reach of the server's per-run Update-ID dedup. The
// successor run starts with the request ID already in carried state — modeled
// here by seeding it — and its validator must reject the replay untouched.
func TestAddPoints_RetryAcrossRoll_RejectedAsDuplicate(t *testing.T) {
	state := enrolledState()
	state.Points = 300
	state.LifetimeEarnEvents = 1
	state.RunNumber = 2
	state.RecentRequestIDs = []string{"req-1"} // recorded by the run that rolled

	env := newEnv(state)

	var rejectErr error
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "update-retry", &testsuite.TestUpdateCallback{
			OnAccept: func() { t.Error("replayed request was accepted") },
			OnReject: func(err error) { rejectErr = err },
		}, rewards.AddPointsRequest{Amount: 300, Reason: "purchase", RequestID: "req-1"})
	}, 0)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateDeactivate, "update-deactivate", &testsuite.TestUpdateCallback{})
	}, time.Millisecond)

	env.ExecuteWorkflow(CustomerRewardsWorkflow, state)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if rejectErr == nil {
		t.Fatal("replayed request was not rejected")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(rejectErr, &appErr) || appErr.Type() != rewards.ErrTypeDuplicateRequest {
		t.Errorf("rejection = %v, want ApplicationError of type %s",
			rejectErr, rewards.ErrTypeDuplicateRequest)
	}

	status := statusOf(t, env)
	if status.Points != 300 {
		t.Errorf("points = %d after replay, want 300 (unchanged)", status.Points)
	}
	if status.LifetimeEarnEvents != 1 {
		t.Errorf("lifetimeEarnEvents = %d after replay, want 1 (unchanged)", status.LifetimeEarnEvents)
	}
}

// A successful add records its request ID, so a second send under the same
// key — even with a distinct Update ID, as a cross-run retry would carry —
// applies nothing.
func TestAddPoints_SameRequestID_AppliedOnce(t *testing.T) {
	state := enrolledState()
	env := newEnv(state)

	var first rewards.AddPointsResult
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "update-1", &testsuite.TestUpdateCallback{
			OnComplete: func(res interface{}, err error) {
				if err != nil {
					t.Errorf("first add failed: %v", err)
					return
				}
				if v, ok := res.(rewards.AddPointsResult); ok {
					first = v
				}
			},
		}, rewards.AddPointsRequest{Amount: 250, Reason: "purchase", RequestID: "req-9"})
	}, 0)

	rejected := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "update-2", &testsuite.TestUpdateCallback{
			OnAccept: func() { t.Error("duplicate request was accepted") },
			OnReject: func(err error) { rejected = true },
		}, rewards.AddPointsRequest{Amount: 250, Reason: "purchase", RequestID: "req-9"})
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateDeactivate, "update-deactivate", &testsuite.TestUpdateCallback{})
	}, 2*time.Millisecond)

	env.ExecuteWorkflow(CustomerRewardsWorkflow, state)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if first.Balance != 250 {
		t.Errorf("first add balance = %d, want 250", first.Balance)
	}
	if !rejected {
		t.Error("second send under req-9 was not rejected")
	}

	status := statusOf(t, env)
	if status.Points != 250 {
		t.Errorf("points = %d, want 250 (applied once)", status.Points)
	}
	if status.LifetimeEarnEvents != 1 {
		t.Errorf("lifetimeEarnEvents = %d, want 1", status.LifetimeEarnEvents)
	}
}

// The ring must actually ride the roll: after EarnsPerRun successful adds the
// run continues as new, and the state it hands the successor carries every
// request ID this run recorded.
func TestContinueAsNew_CarriesRecentRequestIDs(t *testing.T) {
	state := enrolledState()
	env := newEnv(state)

	for i := 0; i < rewards.EarnsPerRun; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		env.RegisterDelayedCallback(func() {
			env.UpdateWorkflow(rewards.UpdateAddPoints, reqID, &testsuite.TestUpdateCallback{
				OnReject: func(err error) { t.Errorf("add %s rejected: %v", reqID, err) },
			}, rewards.AddPointsRequest{Amount: 100, Reason: "purchase", RequestID: reqID})
		}, time.Duration(i)*time.Millisecond)
	}

	env.ExecuteWorkflow(CustomerRewardsWorkflow, state)

	err := env.GetWorkflowError()
	var canErr *workflow.ContinueAsNewError
	if !errors.As(err, &canErr) {
		t.Fatalf("workflow returned %v, want a continue-as-new", err)
	}

	var next rewards.CustomerState
	if err := converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &next); err != nil {
		t.Fatalf("decode carried state: %v", err)
	}

	if next.RunNumber != 2 {
		t.Errorf("carried runNumber = %d, want 2", next.RunNumber)
	}
	want := []string{"req-0", "req-1", "req-2"}
	if len(next.RecentRequestIDs) != len(want) {
		t.Fatalf("carried ring = %v, want %v", next.RecentRequestIDs, want)
	}
	for i, id := range want {
		if next.RecentRequestIDs[i] != id {
			t.Errorf("carried ring[%d] = %q, want %q", i, next.RecentRequestIDs[i], id)
		}
	}
}

// An add that fails in the handler (over the cap) must not consume the key:
// the same request ID stays retryable, and only a later success records it.
func TestAddPoints_FailedAddDoesNotConsumeRequestID(t *testing.T) {
	state := enrolledState()
	state.Points = rewards.PointsCap - 100
	state.LifetimeEarnEvents = 10

	env := newEnv(state)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "update-1", &testsuite.TestUpdateCallback{
			OnComplete: func(res interface{}, err error) {
				if err == nil {
					t.Error("over-cap add succeeded, want a handler failure")
				}
			},
		}, rewards.AddPointsRequest{Amount: 500, Reason: "purchase", RequestID: "req-cap"})
	}, 0)

	// The retry under the same key, for an amount that fits, must be allowed.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "update-2", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { t.Errorf("retry under an unconsumed key rejected: %v", err) },
		}, rewards.AddPointsRequest{Amount: 100, Reason: "purchase", RequestID: "req-cap"})
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateDeactivate, "update-deactivate", &testsuite.TestUpdateCallback{})
	}, 2*time.Millisecond)

	env.ExecuteWorkflow(CustomerRewardsWorkflow, state)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	status := statusOf(t, env)
	if status.Points != rewards.PointsCap {
		t.Errorf("points = %d, want %d", status.Points, rewards.PointsCap)
	}
}
