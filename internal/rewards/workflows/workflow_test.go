package workflows_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestRewardsSuite(t *testing.T) { suite.Run(t, new(RewardsSuite)) }

type RewardsSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

const testCustomerID = "c-001"

func (s *RewardsSuite) SetupTest() { s.env = s.newEnv() }

// newEnv replaces the env's default workflow ID, since the workflow validates
// its payload's customerId against the workflow ID it was started under.
func (s *RewardsSuite) newEnv() *testsuite.TestWorkflowEnvironment {
	env := s.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID:        rewards.WorkflowID(testCustomerID),
		TaskQueue: rewards.TaskQueue,
	})
	return env
}

func (s *RewardsSuite) AfterTest(_, _ string) { s.env.AssertExpectations(s.T()) }

func newState() rewards.CustomerState {
	return rewards.CustomerState{
		CustomerID: testCustomerID,
		Name:       "Ada Lovelace",
		RunNumber:  1,
	}
}

// updateResult captures how an Update resolved: rejected means the validator
// refused it, completed-with-error means the handler ran and failed.
type updateResult struct {
	rejected  error
	completed error
	value     rewards.AddPointsResult
	left      rewards.DeactivateResult
}

func (r *updateResult) callback(s *RewardsSuite) *testsuite.TestUpdateCallback {
	return &testsuite.TestUpdateCallback{
		OnReject: func(err error) { r.rejected = err },
		OnAccept: func() {},
		OnComplete: func(v interface{}, err error) {
			r.completed = err
			if err != nil {
				return
			}
			switch res := v.(type) {
			case rewards.AddPointsResult:
				r.value = res
			case rewards.DeactivateResult:
				r.left = res
			default:
				s.Failf("unexpected update result", "got %T", v)
			}
		},
	}
}

// addPoints schedules an addPoints Update at the given point in workflow time.
func (s *RewardsSuite) addPoints(at time.Duration, id string, req rewards.AddPointsRequest) *updateResult {
	res := &updateResult{}
	s.env.RegisterDelayedCallback(func() {
		s.env.UpdateWorkflow(rewards.UpdateAddPoints, id, res.callback(s), req)
	}, at)
	return res
}

// stopAt cancels the workflow as test teardown, so the long-lived entity
// workflow can finish under the testsuite. Cancellation is not a product path.
func (s *RewardsSuite) stopAt(at time.Duration) {
	s.env.RegisterDelayedCallback(func() { s.env.CancelWorkflow() }, at)
}

func (s *RewardsSuite) deactivateAt(at time.Duration, id string) *updateResult {
	res := &updateResult{}
	s.env.RegisterDelayedCallback(func() {
		s.env.UpdateWorkflow(rewards.UpdateDeactivate, id, res.callback(s))
	}, at)
	return res
}

func (s *RewardsSuite) queryStatusAt(at time.Duration) *rewards.CustomerStatus {
	out := &rewards.CustomerStatus{}
	s.env.RegisterDelayedCallback(func() {
		enc, err := s.env.QueryWorkflow(rewards.QueryGetStatus)
		s.Require().NoError(err)
		s.Require().NoError(enc.Get(out))
	}, at)
	return out
}

func (s *RewardsSuite) runUntilStopped(state rewards.CustomerState) error {
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)
	s.Require().True(s.env.IsWorkflowCompleted())
	return s.env.GetWorkflowError()
}

// continuedState decodes the payload the workflow handed to its successor run.
func (s *RewardsSuite) continuedState() rewards.CustomerState {
	err := s.env.GetWorkflowError()
	var canErr *workflow.ContinueAsNewError
	s.Require().True(errors.As(err, &canErr), "expected a continue-as-new, got %v", err)

	var next rewards.CustomerState
	s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &next))
	return next
}

// --- addPoints ---------------------------------------------------------------

func (s *RewardsSuite) Test_AddPoints_AppliesAndDerivesTier() {
	add := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "signup bonus"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.NoError(add.rejected)
	s.NoError(add.completed)
	s.Equal(500, add.value.Balance)
	s.Equal(rewards.LevelGold, add.value.Level)
}

func (s *RewardsSuite) Test_AddPoints_AccumulatesLifetimeCounters() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 100, Reason: "a"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 250, Reason: "b"})
	status := s.queryStatusAt(3 * time.Minute)
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal(350, status.Points)
	s.Equal(2, status.LifetimeEarnEvents)
}

// A validator rejection is refused before the handler runs, so nothing is
// written to Event History and the customer is untouched.
func (s *RewardsSuite) Test_AddPoints_ValidatorRejects() {
	cases := []struct {
		name string
		req  rewards.AddPointsRequest
		want string
	}{
		{"zero", rewards.AddPointsRequest{Amount: 0, Reason: "x"}, "must be positive"},
		{"negative", rewards.AddPointsRequest{Amount: -50, Reason: "x"}, "must be positive"},
		{"over per-txn max", rewards.AddPointsRequest{Amount: rewards.MaxPointsPerTxn + 1, Reason: "x"}, "per-transaction maximum"},
		{"empty reason", rewards.AddPointsRequest{Amount: 10, Reason: ""}, "reason is required"},
	}

	results := make([]*updateResult, len(cases))
	for i, tc := range cases {
		results[i] = s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i), tc.req)
	}
	status := s.queryStatusAt(time.Duration(len(cases)+1) * time.Minute)
	s.stopAt(time.Duration(len(cases)+2) * time.Minute)

	_ = s.runUntilStopped(newState())

	for i, tc := range cases {
		s.Require().Error(results[i].rejected, "%s should have been rejected by the validator", tc.name)
		s.Contains(results[i].rejected.Error(), tc.want, "%s", tc.name)
		s.NoError(results[i].completed, "%s should never have reached the handler", tc.name)
	}

	s.Equal(0, status.Points)
	s.Equal(0, status.LifetimeEarnEvents)
}

// The points cap is enforced in the handler, so unlike the validator cases the
// attempt is accepted, runs, and its failure is recorded in history.
func (s *RewardsSuite) Test_AddPoints_HandlerRejectsOverPointsCap() {
	state := newState()
	state.Points = rewards.PointsCap - 10
	state.LifetimeEarnEvents = 7

	over := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 11, Reason: "purchase"})
	under := s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 10, Reason: "purchase"})
	status := s.queryStatusAt(3 * time.Minute)
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(state)

	// Accepted by the validator, then failed by the handler, as a typed
	// ApplicationError the API layer can map.
	s.NoError(over.rejected)
	s.Require().Error(over.completed)
	var appErr *temporal.ApplicationError
	s.Require().True(errors.As(over.completed, &appErr))
	s.Equal(rewards.ErrTypePointsCapExceeded, appErr.Type())

	// Landing exactly on the cap is allowed, and the rejected add applied nothing.
	s.NoError(under.completed)
	s.Equal(rewards.PointsCap, status.Points)
	s.Equal(8, status.LifetimeEarnEvents)
}

// --- getStatus ---------------------------------------------------------------

func (s *RewardsSuite) Test_GetStatus_ReportsDerivedFields() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	status := s.queryStatusAt(2 * time.Minute)
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal("c-001", status.CustomerID)
	s.Equal("Ada Lovelace", status.Name)
	s.Equal(600, status.Points)
	s.Equal(rewards.LevelGold, status.Level)
	s.Equal(rewards.PlatinumThreshold, status.NextTierAt)
	s.False(status.EnrolledAt.IsZero(), "EnrolledAt is stamped on the first run")
}

// --- Enrollment validation ---------------------------------------------------

// The workflow is the only integrity boundary, so an incoherent start payload
// fails the execution rather than starting a customer whose numbers don't add up.
func (s *RewardsSuite) Test_Enroll_RejectsBadPayload() {
	cases := []struct {
		name  string
		mutar func(*rewards.CustomerState)
		want  string
	}{
		{"customerId disagrees with workflow ID",
			func(st *rewards.CustomerState) { st.CustomerID = "someone-else" },
			"does not match workflow ID"},
		{"empty name",
			func(st *rewards.CustomerState) { st.Name = "" },
			"name is required"},
		{"seeded above the points cap",
			func(st *rewards.CustomerState) {
				st.Points = rewards.PointsCap + 1
				st.LifetimeEarnEvents = 1
			},
			"exceeds the cap"},
		{"negative points",
			func(st *rewards.CustomerState) { st.Points = -1 },
			"non-negative"},
		{"zero runNumber",
			func(st *rewards.CustomerState) { st.RunNumber = 0 },
			"at least 1"},
		{"points earned with no earn events",
			func(st *rewards.CustomerState) { st.Points = 10 },
			"lifetimeEarnEvents is 0"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			env := s.newEnv()

			state := newState()
			tc.mutar(&state)
			env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

			s.Require().True(env.IsWorkflowCompleted())
			err := env.GetWorkflowError()
			s.Require().Error(err)
			s.Contains(err.Error(), tc.want)

			var appErr *temporal.ApplicationError
			s.Require().True(errors.As(err, &appErr))
			s.Equal(rewards.ErrTypeInvalidEnrollment, appErr.Type())
			s.True(appErr.NonRetryable(), "a bad payload will not become good on retry")
		})
	}
}

// --- Continue-as-new ---------------------------------------------------------

func (s *RewardsSuite) Test_ContinueAsNew_FiresOnTheNthAdd() {
	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	// No cancel scheduled: the roll itself is what ends this run.
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, newState())

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()
	s.Equal(2, next.RunNumber)
}

// What survives the rollover. History is reaped after retention; the carried
// payload is not, so anything not in here is gone for good.
func (s *RewardsSuite) Test_ContinueAsNew_CarriesStateForward() {
	enrolled := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	state := newState()
	state.EnrolledAt = enrolled
	state.RunNumber = 4
	state.Points = 200
	state.LifetimeEarnEvents = 9

	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()

	s.Equal(5, next.RunNumber, "the run number increments exactly once per roll")
	s.Equal(500, next.Points, "200 carried + 3x100 earned this run")
	s.Equal(12, next.LifetimeEarnEvents, "9 carried + 3 this run")
	s.True(next.EnrolledAt.Equal(enrolled), "original enrollment survives untouched")
	s.Equal("c-001", next.CustomerID)
	s.Equal("Ada Lovelace", next.Name)
}

// --- Deactivation ------------------------------------------------------------

// Deactivation is one-way: the leave commits, the run drains its handlers, and
// the workflow completes normally with the balance frozen in final state.
func (s *RewardsSuite) Test_Deactivate_CompletesTheWorkflow() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	deact := s.deactivateAt(2*time.Minute, "leave")
	// No stopAt: the deactivate itself is what ends the run.
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, newState())

	s.Require().True(s.env.IsWorkflowCompleted())
	s.Require().NoError(deact.rejected)
	s.Require().NoError(deact.completed)
	s.True(deact.left.Changed, "the leave is a real transition")

	err := s.env.GetWorkflowError()
	var canErr *workflow.ContinueAsNewError
	s.False(errors.As(err, &canErr), "deactivation must complete the run, not roll it")
	s.NoError(err, "leaving the program is a normal completion, not a failure")

	enc, qerr := s.env.QueryWorkflow(rewards.QueryGetStatus)
	s.Require().NoError(qerr)
	var status rewards.CustomerStatus
	s.Require().NoError(enc.Get(&status))
	s.False(status.Active)
	s.Equal(600, status.Points, "the balance is frozen, not erased")
}

// The drain-window guards -- the Changed=false answer to a duplicate
// deactivate and the ErrTypeDeactivated rejection of an addPoints that races
// the leave -- are deliberately untested here: the test environment applies
// Updates one at a time and silently drops anything sent after the run
// completes, so a test would assert on callbacks that never ran.
