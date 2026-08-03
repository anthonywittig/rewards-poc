package workflows_test

import (
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

func TestAddPoints_Sums(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID:        rewards.WorkflowID("c-001"),
		TaskQueue: rewards.TaskQueue,
	})

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "u1", &testsuite.TestUpdateCallback{},
			rewards.AddPointsRequest{Amount: 100, Reason: "a"})
	}, time.Minute)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(rewards.UpdateAddPoints, "u2", &testsuite.TestUpdateCallback{},
			rewards.AddPointsRequest{Amount: 250, Reason: "b"})
	}, 2*time.Minute)

	var status rewards.CustomerStatus
	env.RegisterDelayedCallback(func() {
		enc, err := env.QueryWorkflow(rewards.QueryGetStatus)
		require.NoError(t, err)
		require.NoError(t, enc.Get(&status))
	}, 3*time.Minute)

	// The entity workflow otherwise waits forever; cancel is test teardown only.
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 4*time.Minute)

	env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, rewards.CustomerState{
		CustomerID: "c-001",
		Name:       "Ada Lovelace",
		RunNumber:  1,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, 350, status.Points)
}
