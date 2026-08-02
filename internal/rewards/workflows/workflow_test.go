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

// newEnv builds a test environment the workflow will actually run in. The
// workflow validates its payload's customerId against the workflow ID it was
// started under, so the env's "default-test-workflow-id" has to be replaced.
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
	}
}

// updateResult captures how an Update actually resolved. rejected means the
// validator refused and nothing was written to history; completed-with-error
// means the handler ran and the failure *is* recorded.
type updateResult struct {
	rejected  error
	completed error
	value     rewards.AddPointsResult
	left      rewards.DeactivateResult
	rejoined  rewards.ReactivateResult
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
			// Enumerated rather than type-asserted to one, so a handler that
			// starts returning something else fails here instead of leaving
			// the assertion reading a zero value -- which for the two
			// membership results means Changed=false, the answer half these
			// tests are trying to distinguish.
			switch res := v.(type) {
			case rewards.AddPointsResult:
				r.value = res
			case rewards.DeactivateResult:
				r.left = res
			case rewards.ReactivateResult:
				r.rejoined = res
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

// stopAt schedules CancelWorkflow as test-env teardown only so the long-running
// entity workflow can finish under the testsuite. Product leave is soft-deactivate
// (see deactivateAt); CancelWorkflow is not a product path.
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

func (s *RewardsSuite) reactivateAt(at time.Duration, id string) *updateResult {
	res := &updateResult{}
	s.env.RegisterDelayedCallback(func() {
		s.env.UpdateWorkflow(rewards.UpdateReactivate, id, res.callback(s))
	}, at)
	return res
}

// queryStatusAt reads getStatus mid-run.
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

// continuedState decodes the payload the workflow handed to its successor run,
// which is the only way to assert what actually survives a rollover.
func (s *RewardsSuite) continuedState() rewards.CustomerState {
	err := s.env.GetWorkflowError()
	var canErr *workflow.ContinueAsNewError
	s.Require().True(errors.As(err, &canErr), "expected a continue-as-new, got %v", err)

	var next rewards.CustomerState
	s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &next))
	return next
}

// --- addPoints happy path ---------------------------------------------------

func (s *RewardsSuite) Test_AddPoints_AppliesAndDerivesTier() {
	add := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "signup bonus"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.NoError(add.rejected)
	s.NoError(add.completed)
	s.Equal(500, add.value.Balance)
	s.Equal(rewards.LevelGold, add.value.Level)
}

// The tier boundary crossed through the real handler, not just the pure
// function: 499 is still basic, one more point promotes.
func (s *RewardsSuite) Test_AddPoints_CrossesGoldBoundary() {
	first := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 499, Reason: "purchase"})
	second := s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 1, Reason: "purchase"})
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal(rewards.LevelBasic, first.value.Level, "499 points is still basic")
	s.Equal(rewards.LevelGold, second.value.Level, "500 points promotes to gold")
	s.Equal(500, second.value.Balance)
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

// --- Validator rejections --------------------------------------------------

// Each of these is refused before the handler runs, so nothing is written to
// Event History at all. The unit test can only observe the rejection itself;
// that no *events* were written is demonstrated against the real server in the
// README walkthrough, since the test environment has no history to inspect.
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

	// The customer is untouched -- no partial application, no counter bump.
	s.Equal(0, status.Points)
	s.Equal(0, status.LifetimeEarnEvents)
	s.Equal(rewards.LevelBasic, status.Level)
}

// The per-transaction maximum is exactly inclusive: 1000 is allowed, 1001 is not.
func (s *RewardsSuite) Test_AddPoints_PerTxnMaxIsInclusive() {
	ok := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: rewards.MaxPointsPerTxn, Reason: "big"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.NoError(ok.rejected)
	s.NoError(ok.completed)
	s.Equal(rewards.MaxPointsPerTxn, ok.value.Balance)
}

// --- Handler-side business rejection ---------------------------------------

// The points cap is enforced in the handler, so unlike the validator cases this
// attempt is accepted, runs, and its failure is recorded in history -- which is
// the point: a support rep asking "why didn't they reach platinum?" gets an
// answer.
func (s *RewardsSuite) Test_AddPoints_HandlerRejectsOverPointsCap() {
	state := newState()
	state.Points = rewards.PointsCap - 10
	state.LifetimeEarnEvents = 7

	over := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 11, Reason: "purchase"})
	under := s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 10, Reason: "purchase"})
	status := s.queryStatusAt(3 * time.Minute)
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(state)

	// Accepted by the validator, then failed by the handler.
	s.NoError(over.rejected, "the points cap must not be enforced in the validator")
	s.Require().Error(over.completed)

	var appErr *temporal.ApplicationError
	s.Require().True(errors.As(over.completed, &appErr), "want a typed ApplicationError for the API layer to map")
	s.Equal(rewards.ErrTypePointsCapExceeded, appErr.Type())

	// Landing exactly on the cap is allowed.
	s.NoError(under.completed)

	// The rejected add applied nothing; only the successful one counted.
	s.Equal(rewards.PointsCap, status.Points)
	s.Equal(8, status.LifetimeEarnEvents)
}

// --- getStatus query -------------------------------------------------------

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

// At the top tier there is no next tier; the wire value is 0 rather than a
// misleading threshold.
func (s *RewardsSuite) Test_GetStatus_NoNextTierAtPlatinum() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: rewards.PlatinumThreshold, Reason: "purchase"})
	status := s.queryStatusAt(2 * time.Minute)
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal(rewards.LevelPlatinum, status.Level)
	s.Equal(0, status.NextTierAt)
}

// Enrollment carries a prior EnrolledAt untouched, which is what makes the
// value survive continue-as-new.
func (s *RewardsSuite) Test_GetStatus_PreservesCarriedEnrolledAt() {
	enrolled := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	state := newState()
	state.EnrolledAt = enrolled
	state.Generation = 4

	status := s.queryStatusAt(time.Minute)
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(state)

	s.True(status.EnrolledAt.Equal(enrolled), "got %s, want %s", status.EnrolledAt, enrolled)
	s.Equal(4, status.Generation)
}

// --- Enrollment validation ---------------------------------------------------

// The workflow is the only integrity boundary -- there is no database schema
// behind it -- so an incoherent start payload has to be rejected here or not at
// all. These fail the execution outright rather than starting a customer whose
// numbers do not add up.
func (s *RewardsSuite) Test_Enroll_RejectsBadPayload() {
	cases := []struct {
		name  string
		mutar func(*rewards.CustomerState)
		want  string
	}{
		{"customerId disagrees with workflow ID",
			func(st *rewards.CustomerState) { st.CustomerID = "someone-else" },
			"does not match workflow ID"},
		{"empty customerId",
			func(st *rewards.CustomerState) { st.CustomerID = "" },
			"does not match workflow ID"},
		{"empty name",
			func(st *rewards.CustomerState) { st.Name = "" },
			"name is required"},
		{"name is only whitespace",
			func(st *rewards.CustomerState) { st.Name = "   " },
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
		{"negative generation",
			func(st *rewards.CustomerState) { st.Generation = -1 },
			"non-negative"},
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
			s.Require().Error(err, "an incoherent enrollment must not produce a running customer")
			s.Contains(err.Error(), tc.want)

			var appErr *temporal.ApplicationError
			s.Require().True(errors.As(err, &appErr))
			s.Equal(rewards.ErrTypeInvalidEnrollment, appErr.Type())
			s.True(appErr.NonRetryable(), "a bad payload will not become good on retry")
		})
	}
}

// A large seeded balance alongside a zero lifetime total once bypassed the
// handler's cap check. Kept as the regression guard.
func (s *RewardsSuite) Test_Enroll_RejectsCapBypass() {
	state := newState()
	state.Points = 5_000_000
	state.LifetimeEarnEvents = 1

	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	s.Require().Error(s.env.GetWorkflowError())
}

// A seeded mid-life customer is legitimate -- the balance just has to be
// consistent with having earned it.
func (s *RewardsSuite) Test_Enroll_AcceptsSeededBalance() {
	state := newState()
	state.Points = 900
	state.LifetimeEarnEvents = 6

	status := s.queryStatusAt(time.Minute)
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(state)

	s.Equal(900, status.Points)
	s.Equal(6, status.LifetimeEarnEvents)
	s.Equal(rewards.LevelGold, status.Level)
}

// --- Continue-as-new -------------------------------------------------------

// The roll fires on exactly the Nth add, not before.
func (s *RewardsSuite) Test_ContinueAsNew_FiresOnTheNthAdd() {
	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	// No cancel scheduled: the roll itself is what ends this run.
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, newState())

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()
	s.Equal(1, next.Generation)
}

// One short of the threshold, the run is still going -- so the exit really is
// driven by the counter rather than by anything incidental. stopAt is test-env
// teardown only so the entity workflow can finish under the testsuite.
func (s *RewardsSuite) Test_ContinueAsNew_DoesNotFireEarly() {
	for i := 0; i < rewards.EarnsPerRun-1; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.stopAt(time.Duration(rewards.EarnsPerRun+1) * time.Minute)

	err := s.runUntilStopped(newState())

	// Stopped via teardown, not continued-as-new.
	var canErr *workflow.ContinueAsNewError
	s.False(errors.As(err, &canErr), "should not have rolled on %d adds", rewards.EarnsPerRun-1)
	var canceled *temporal.CanceledError
	s.True(errors.As(err, &canceled))
}

// What survives the rollover. This is the part the audit log depends on:
// history is reaped, the carried payload is not, so anything not in here is
// gone for good.
func (s *RewardsSuite) Test_ContinueAsNew_CarriesStateForward() {
	enrolled := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	state := newState()
	state.EnrolledAt = enrolled
	state.Generation = 4
	state.Points = 200
	state.LifetimeEarnEvents = 9

	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()

	s.Equal(5, next.Generation, "generation increments exactly once per roll")
	s.Equal(500, next.Points, "200 carried + 3x100 earned this run")
	s.Equal(12, next.LifetimeEarnEvents, "9 carried + 3 this run")
	s.True(next.EnrolledAt.Equal(enrolled), "original enrollment survives untouched")
	s.Equal("c-001", next.CustomerID)
	s.Equal("Ada Lovelace", next.Name)
}

// --- Soft deactivation -----------------------------------------------------

// The full leave/rejoin round trip. Soft-deactivate keeps the workflow running
// with the balance intact; reactivate clears the flag, restores the same
// points, and the customer earns again on top of them. The reactivate Update
// takes no argument at all, so there is no path by which rejoining can rewrite
// the customer's name -- the ID is derived from that name, so a re-enrollment
// only reaches this workflow when the name it arrived under already slugs to
// this customer's ID.
func (s *RewardsSuite) Test_SoftDeactivate_ThenReactivate_RestoresEverything() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	deact := s.deactivateAt(2*time.Minute, "leave")
	statusAfterLeave := s.queryStatusAt(3 * time.Minute)
	rejoin := s.reactivateAt(4*time.Minute, "rejoin")
	statusAfterRejoin := s.queryStatusAt(5 * time.Minute)
	after := s.addPoints(6*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(7 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(deact.rejected)
	s.Require().NoError(deact.completed)
	s.True(deact.left.Changed, "the first leave is a real transition")
	s.False(statusAfterLeave.Active, "soft leave must mark inactive")
	s.Equal(600, statusAfterLeave.Points, "points must survive deactivation")

	s.Require().NoError(rejoin.completed)
	s.True(rejoin.rejoined.Changed)
	s.True(statusAfterRejoin.Active, "the customer is a member again")
	s.Equal("Ada Lovelace", statusAfterRejoin.Name, "rejoining cannot rename the customer")
	s.Equal(600, statusAfterRejoin.Points, "re-enrollment is not a reset")
	s.Equal(1, statusAfterRejoin.LifetimeEarnEvents, "nor does it forget how the balance was earned")
	s.Equal(rewards.LevelGold, statusAfterRejoin.Level)

	s.Require().NoError(after.rejected)
	s.Require().NoError(after.completed, "a rejoined customer must be able to earn again")
	s.Equal(700, after.value.Balance, "and they earn on top of the restored balance")
}

// Both membership Updates are idempotent, and both have to *say* so: the API
// turns a repeat DELETE into 204 and a duplicate enrollment into 409 on the
// strength of Changed, and the audit timeline draws a row only when it is true.
// Reporting Changed=true for a no-op would show a customer leaving twice.
func (s *RewardsSuite) Test_SoftDeactivate_RepeatIsANoOp() {
	first := s.deactivateAt(time.Minute, "leave-1")
	second := s.deactivateAt(2*time.Minute, "leave-2")
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(first.completed)
	s.Require().NoError(second.completed)
	s.True(first.left.Changed, "the first leave is a real transition")
	s.False(second.left.Changed, "the second changed nothing")
}

// Re-enrolling someone who never left is a duplicate signup, and the handler
// reports rather than applies it. Applying would restart a membership that never
// ended -- the enroll endpoint depends on Changed=false to turn this into the
// 409 it owes the caller.
func (s *RewardsSuite) Test_Reactivate_OnAnActiveCustomerChangesNothing() {
	rejoin := s.reactivateAt(time.Minute, "rejoin")
	status := s.queryStatusAt(2 * time.Minute)
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(rejoin.completed)
	s.False(rejoin.rejoined.Changed, "an active customer was not reactivated")
	s.Equal("Ada Lovelace", status.Name, "a duplicate enroll must not rename the customer")
	s.True(status.Active)
}

// addPoints against a soft-deactivated customer is rejected with ErrTypeDeactivated.
func (s *RewardsSuite) Test_SoftDeactivate_RejectsAddPoints() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	blocked := s.addPoints(3*time.Minute, "u2", rewards.AddPointsRequest{Amount: 50, Reason: "purchase"})
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().Error(blocked.completed)
	var app *temporal.ApplicationError
	s.Require().True(errors.As(blocked.completed, &app))
	s.Equal(rewards.ErrTypeDeactivated, app.Type())
}
