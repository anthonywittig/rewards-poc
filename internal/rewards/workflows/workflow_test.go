package workflows_test

import (
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

// A retry that straddles a continue-as-new arrives with an Update ID the new
// run's server-side dedup has never seen; the carried ledger must answer it
// with the original result instead of applying it again.
func TestAddPointsDuplicateAcrossContinueAsNew(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "ada"})

	// Run 2 of a chain, as continue-as-new would start it: req-1 was applied
	// during run 1 and travels in the ledger.
	state := rewards.CustomerState{
		CustomerID:         "ada",
		Name:               "Ada",
		Points:             600,
		Active:             true,
		LifetimeEarnEvents: 3,
		RunNumber:          2,
		RecentRequests: []rewards.AppliedRequest{
			{RequestID: "req-1", Balance: 600, Level: rewards.LevelGold},
		},
	}

	var dup, fresh rewards.AddPointsResult
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "req-1", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { t.Errorf("duplicate rejected: %v", err) },
			OnComplete: func(success interface{}, err error) {
				if err != nil {
					t.Errorf("duplicate failed: %v", err)
					return
				}
				dup = success.(rewards.AddPointsResult)
			},
		}, rewards.AddPointsRequest{Amount: 400, Reason: "retry of an applied add"})
	}, 0)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "req-2", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { t.Errorf("fresh add rejected: %v", err) },
			OnComplete: func(success interface{}, err error) {
				if err != nil {
					t.Errorf("fresh add failed: %v", err)
					return
				}
				fresh = success.(rewards.AddPointsResult)
			},
		}, rewards.AddPointsRequest{Amount: 400, Reason: "purchase"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateDeactivate, "req-3", &testsuite.TestUpdateCallback{})
	}, 2*time.Second)

	env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	if dup.Balance != 600 || dup.Level != rewards.LevelGold {
		t.Errorf("duplicate got %+v, want the original result (balance 600, gold)", dup)
	}
	if fresh.Balance != 1000 || fresh.Level != rewards.LevelPlatinum {
		t.Errorf("fresh add got %+v, want balance 1000, platinum", fresh)
	}

	val, err := env.QueryWorkflow(rewards.QueryGetStatus)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var status rewards.CustomerStatus
	if err := val.Get(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Points != 1000 {
		t.Errorf("points = %d, want 1000 (the duplicate must not double-apply)", status.Points)
	}
	if status.LifetimeEarnEvents != 4 {
		t.Errorf("lifetimeEarnEvents = %d, want 4", status.LifetimeEarnEvents)
	}
}
