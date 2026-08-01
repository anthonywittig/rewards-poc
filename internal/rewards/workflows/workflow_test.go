package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/activities"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"github.com/stretchr/testify/mock"
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

// testActivities is the struct the test env registers, and the receiver every
// OnActivity mock names its method on.
//
// One shared instance rather than one per env: the mocks below replace the
// method body outright, so nothing here is ever actually called, and the SDK
// only needs the method value to resolve the Activity's registered name. A
// per-test instance with real dependencies would be the shape to use the day an
// Activity is exercised for real rather than mocked.
var testActivities = &activities.Activities{}

func (s *RewardsSuite) SetupTest() { s.env = s.newEnv() }

// newEnv builds a test environment the workflow will actually run in.
//
// Two things every test needs. The workflow validates that its payload's
// customerId matches the workflow ID it was started under, so the env's
// "default-test-workflow-id" has to be replaced with a real one. And since
// Phase 6 the workflow schedules an Activity on soft-deactivate departure, so
// an env with none registered fails those paths with "no activity is registered
// for taskqueue 'rewards'" -- which is the test environment being right: the
// workflow does now have a side effect, and pretending otherwise would hide it.
func (s *RewardsSuite) newEnv() *testsuite.TestWorkflowEnvironment {
	env := s.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID:        rewards.WorkflowID(testCustomerID),
		TaskQueue: rewards.TaskQueue,
	})
	env.RegisterActivity(testActivities)
	return env
}

func (s *RewardsSuite) AfterTest(_, _ string) { s.env.AssertExpectations(s.T()) }

func newState() rewards.CustomerState {
	return rewards.CustomerState{
		CustomerID: testCustomerID,
		Name:       "Ada Lovelace",
		Email:      "ada@example.com",
	}
}

// updateResult captures how an Update actually resolved. The three outcomes are
// distinct and the distinction is the whole point of PLAN.md 3.4: rejected means
// the validator refused and nothing was written to history; completed-with-error
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
			// The test env hands back the concrete value the handler returned,
			// and there are three of them now. Enumerated rather than
			// type-asserted to one, so a handler that starts returning
			// something else fails here instead of silently leaving the
			// assertion reading a zero value -- which for DeactivateResult and
			// ReactivateResult means Changed=false, the answer half these tests
			// are trying to distinguish.
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

func (s *RewardsSuite) reactivateAt(at time.Duration, id string, req rewards.ReactivateRequest) *updateResult {
	res := &updateResult{}
	s.env.RegisterDelayedCallback(func() {
		s.env.UpdateWorkflow(rewards.UpdateReactivate, id, res.callback(s), req)
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
	s.Equal("c-001:1", add.value.EventID)
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

// --- Validator rejections (PLAN.md 3.4) -------------------------------------

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

// --- Handler-side business rejection (PLAN.md 3.4) --------------------------

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

// --- getStatus query (PLAN.md 3.3) ------------------------------------------

func (s *RewardsSuite) Test_GetStatus_ReportsDerivedFields() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	status := s.queryStatusAt(2 * time.Minute)
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal("c-001", status.CustomerID)
	s.Equal("Ada Lovelace", status.Name)
	s.Equal("ada@example.com", status.Email)
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
// value survive continue-as-new in Phase 2.
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

// The bypass Bugbot found on PR #4: seed a large balance alongside a zero
// lifetime total, and the handler's cap check has nothing to push against.
// Collapsing the two fields removed the second number entirely, so the cap is
// now measured against the only balance there is -- but the payload is still
// rejected at the door, and this test stays as the regression guard.
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

// --- Points are monotonic ----------------------------------------------------

// The invariant the whole state shape now rests on: points only ever increase.
// There is no spend, redeem, expire or adjust path, so no sequence of legal
// operations can lower a balance. If a redemption feature ever arrives, this
// test is the one that should fail first.
func (s *RewardsSuite) Test_Points_OnlyEverIncrease() {
	// Kept to one run's worth of adds: beyond EarnsPerRun the run rolls over and
	// further updates belong to the next run, which is Phase 2's concern.
	adds := []int{10, 250, 1}

	results := make([]*updateResult, len(adds))
	for i, amount := range adds {
		results[i] = s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: amount, Reason: "purchase"})
	}
	s.stopAt(time.Duration(len(adds)+1) * time.Minute)

	_ = s.runUntilStopped(newState())

	prev, want := 0, 0
	for i, amount := range adds {
		s.Require().NoError(results[i].completed)
		want += amount
		s.Equal(want, results[i].value.Balance, "add %d", i)
		s.Greater(results[i].value.Balance, prev, "balance must strictly increase on every add")
		prev = results[i].value.Balance
	}
}

// --- Continue-as-new (PLAN.md 3.5) ------------------------------------------

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
// gone for good. PLAN.md 6.3.
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
	s.Equal("ada@example.com", next.Email)
}

// The per-run counter is a local, so the successor run starts at zero rather
// than rolling again immediately. Asserted by running the successor's payload
// through the workflow again and watching it take another full N adds.
func (s *RewardsSuite) Test_ContinueAsNew_ResetsPerRunCounter() {
	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, newState())
	next := s.continuedState()

	// Second run, same customer, one add short of the threshold.
	env2 := s.newEnv()
	for i := 0; i < rewards.EarnsPerRun-1; i++ {
		i := i
		env2.RegisterDelayedCallback(func() {
			env2.UpdateWorkflow(rewards.UpdateAddPoints, fmt.Sprintf("v%d", i),
				&testsuite.TestUpdateCallback{
					OnAccept: func() {}, OnReject: func(error) {}, OnComplete: func(interface{}, error) {},
				}, rewards.AddPointsRequest{Amount: 10, Reason: "purchase"})
		}, time.Duration(i+1)*time.Minute)
	}
	// CancelWorkflow is test-env teardown only; not a product leave path.
	env2.RegisterDelayedCallback(env2.CancelWorkflow, time.Duration(rewards.EarnsPerRun+1)*time.Minute)
	env2.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, next)

	s.Require().True(env2.IsWorkflowCompleted())
	var canErr *workflow.ContinueAsNewError
	s.False(errors.As(env2.GetWorkflowError(), &canErr),
		"the successor run must start its own count, not inherit a primed one")
}

// --- Soft deactivation (PLAN.md 3.6) ----------------------------------------

// Soft-deactivate keeps the workflow running with the balance intact;
// reactivate clears the flag and restores the same points.
func (s *RewardsSuite) Test_SoftDeactivate_KeepsPointsForReenroll() {
	s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	deact := s.deactivateAt(2*time.Minute, "leave")
	statusAfterLeave := s.queryStatusAt(3 * time.Minute)
	s.reactivateAt(4*time.Minute, "rejoin", rewards.ReactivateRequest{
		Name: "Ada Lovelace", Email: "ada@example.com",
	})
	statusAfterRejoin := s.queryStatusAt(5 * time.Minute)
	s.stopAt(6 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(deact.rejected)
	s.Require().NoError(deact.completed)
	s.True(deact.left.Changed, "the first leave is a real transition")
	s.False(statusAfterLeave.Active, "soft leave must mark inactive")
	s.Equal(600, statusAfterLeave.Points, "points must survive deactivation")
	s.True(statusAfterRejoin.Active, "re-enroll must reactivate")
	s.Equal(600, statusAfterRejoin.Points, "re-enroll must restore the prior balance")
}

// Both membership Updates are idempotent, and both have to *say* so: the API
// turns a repeat DELETE into 204 and a duplicate enrollment into 409 on the
// strength of Changed, and the audit timeline draws a row only when it is true.
// Reporting Changed=true for a no-op would show a customer leaving twice.
func (s *RewardsSuite) Test_SoftDeactivate_RepeatIsANoOp() {
	s.mockNotify(0)

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
// reports rather than applies it. Applying would let a second signup silently
// overwrite a live customer's name and email -- the enroll endpoint depends on
// Changed=false to turn this into the 409 it owes the caller.
func (s *RewardsSuite) Test_Reactivate_OnAnActiveCustomerChangesNothing() {
	s.mockNotify(0)

	rejoin := s.reactivateAt(time.Minute, "rejoin", rewards.ReactivateRequest{
		Name: "Mallory", Email: "mallory@example.com",
	})
	status := s.queryStatusAt(2 * time.Minute)
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(rejoin.completed)
	s.False(rejoin.rejoined.Changed, "an active customer was not reactivated")
	s.Equal("Ada Lovelace", status.Name, "a duplicate enroll must not rename the customer")
	s.Equal("ada@example.com", status.Email, "nor take over their email")
	s.True(status.Active)
}

// Re-enrollment takes the new name and email -- the customer signed up again,
// possibly with different details -- while leaving everything that makes the
// balance meaningful alone.
func (s *RewardsSuite) Test_Reactivate_AdoptsNewContactDetailsAndKeepsCounters() {
	s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	rejoin := s.reactivateAt(3*time.Minute, "rejoin", rewards.ReactivateRequest{
		Name: "Ada King", Email: "ada.king@example.com",
	})
	status := s.queryStatusAt(4 * time.Minute)
	s.stopAt(5 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(rejoin.completed)
	s.True(rejoin.rejoined.Changed)
	s.Equal("Ada King", status.Name)
	s.Equal("ada.king@example.com", status.Email)
	s.Equal(600, status.Points, "re-enrollment is not a reset")
	s.Equal(1, status.LifetimeEarnEvents, "nor does it forget how the balance was earned")
	s.Equal(rewards.LevelGold, status.Level)
}

// A rejoined customer can earn again. Without this, "restore the balance" would
// be a display-only claim: the handler's deactivated guard reads the same flag
// reactivate clears, so a clear that did not take shows up here as a 409.
func (s *RewardsSuite) Test_Reactivate_RestoresTheAbilityToEarn() {
	s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	s.reactivateAt(3*time.Minute, "rejoin", rewards.ReactivateRequest{
		Name: "Ada Lovelace", Email: "ada@example.com",
	})
	after := s.addPoints(4*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(5 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Require().NoError(after.rejected)
	s.Require().NoError(after.completed, "a rejoined customer must be able to earn again")
	s.Equal(700, after.value.Balance, "and they earn on top of the restored balance")
}

// Leaving does not re-announce a tier on the way back. NotifiedLevels is carried
// through the round trip, so a customer who was congratulated for gold before
// leaving is not congratulated for it again on re-enrollment.
func (s *RewardsSuite) Test_Reactivate_DoesNotRenotifyACarriedLevel() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	s.reactivateAt(3*time.Minute, "rejoin", rewards.ReactivateRequest{
		Name: "Ada Lovelace", Email: "ada@example.com",
	})
	s.addPoints(4*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(5 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"gold was announced before the customer left; rejoining is not a new promotion")
}

// The departure notice is armed by the transition, not by the request, so an
// idempotent repeat does not send a second one. The customer left once.
func (s *RewardsSuite) Test_SoftDeactivate_RepeatSendsOneDepartureNotice() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave-1")
	s.deactivateAt(3*time.Minute, "leave-2")
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventDeparted),
		"a repeat DELETE must not notify the customer a second time")
}

// addPoints against a soft-deactivated customer is rejected with ErrTypeDeactivated.
func (s *RewardsSuite) Test_SoftDeactivate_RejectsAddPoints() {
	s.mockNotify(0)

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

// Soft deactivate still sends the departure notification.
func (s *RewardsSuite) Test_SoftDeactivate_SendsDepartureNotice() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted))
	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventDeparted))
}

// --- Tier promotion notifications (PLAN.md 3.7) -----------------------------

// notifyCalls records what the mocked Activity was actually asked to send.
type notifyCalls struct {
	mu   sync.Mutex
	reqs []rewards.NotifyRequest
}

func (c *notifyCalls) add(req rewards.NotifyRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
}

func (c *notifyCalls) all() []rewards.NotifyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rewards.NotifyRequest(nil), c.reqs...)
}

func (c *notifyCalls) levels(event string) []string {
	var out []string
	for _, r := range c.all() {
		if r.Event == event {
			out = append(out, r.Level)
		}
	}
	return out
}

// mockNotify installs the Activity mock with one delay for every delivery.
// A delay keeps a delivery genuinely in flight, which is what makes the
// continue-as-new race reachable rather than theoretical.
func (s *RewardsSuite) mockNotify(delay time.Duration) *notifyCalls {
	return s.mockNotifyPer(func(rewards.NotifyRequest) time.Duration { return delay })
}

// mockNotifyPer varies the delay by request, which some tests need: a guard that
// waits for delivery A is only exercised if delivery B can finish first.
func (s *RewardsSuite) mockNotifyPer(delay func(rewards.NotifyRequest) time.Duration) *notifyCalls {
	calls := &notifyCalls{}
	s.env.OnActivity(testActivities.NotifyCustomer, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req rewards.NotifyRequest) error {
			if d := delay(req); d > 0 {
				time.Sleep(d)
			}
			calls.add(req)
			return nil
		}).Maybe()
	return calls
}

// A promotion landing on the *third* add is the ordinary case at
// EarnsPerRun = 3, and it is precisely when the run wants to continue as new.
// The main loop drains needsNotify before rolling, so the promotion is sent in
// this run and NotifiedLevels rides into the successor. (The earlier
// workflow.Go design needed an explicit notifier.idle() guard for the same
// reason -- PLAN.md 12.6 -- and this test failed without it.)
func (s *RewardsSuite) Test_Notify_PromotionOnTheRollingAddIsNotDropped() {
	calls := s.mockNotify(50 * time.Millisecond)

	// 200 + 200 + 200 = 600: basic until the third add, gold on it.
	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 200, Reason: "purchase"})
	}
	// No cancel: the roll is what ends this run.
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, newState())

	s.Require().True(s.env.IsWorkflowCompleted())
	s.Require().Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"expected the promotion to survive the roll, got %v", calls.all())

	// ...and the successor must know it was sent, or the next run re-notifies.
	next := s.continuedState()
	s.Equal([]string{rewards.LevelGold}, next.NotifiedLevels)
}

// A promotion that is not racing the roll takes the ordinary path.
func (s *RewardsSuite) Test_Notify_PromotionMidRunIsSent() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted))
}

// Both boundaries crossed inside one run produce two notifications, in order.
// The main loop sends one promotion per wake; a second add that advances the
// tier re-arms needsNotify so platinum is not lost behind gold.
func (s *RewardsSuite) Test_Notify_BothTiersInOneRun() {
	calls := s.mockNotify(20 * time.Millisecond)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold, rewards.LevelPlatinum},
		calls.levels(rewards.NotifyEventPromoted))
}

// An add that stays inside a tier notifies nobody. The obvious case, and the one
// that would make the demo unbearable if it were wrong.
func (s *RewardsSuite) Test_Notify_NoPromotionWithinATier() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Empty(calls.levels(rewards.NotifyEventPromoted))
}

// The at-least-once dedup guard: a level already in NotifiedLevels is not
// re-sent.
//
// Honest about what this is. Points only go up, so a customer cannot fall out of
// gold and climb back in, which means the state below is not reachable by any
// sequence of legal operations today -- it is constructed. The guard is here
// because Activities are at-least-once and because the day a spend or expiry
// path lands, this is the check that stops a customer being congratulated twice.
// PLAN.md 3.7.
func (s *RewardsSuite) Test_Notify_DoesNotRenotifyACarriedLevel() {
	calls := s.mockNotify(0)

	state := newState()
	state.Points = 400
	state.LifetimeEarnEvents = 4
	state.NotifiedLevels = []string{rewards.LevelGold}

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 200, Reason: "purchase"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(state)

	s.Empty(calls.levels(rewards.NotifyEventPromoted),
		"gold was already notified; crossing it again must not re-send")
}

// Departure reuses the same Activity, which is why there is no separate cleanup
// Activity in the design. PLAN.md 3.7. Product leave is soft-deactivate.
func (s *RewardsSuite) Test_Notify_DepartureUsesTheSameActivity() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	s.deactivateAt(2*time.Minute, "leave")
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	sent := calls.all()
	s.Require().Len(sent, 2, "expected a promotion and a departure, got %v", sent)

	departed := sent[1]
	s.Equal(rewards.NotifyEventDeparted, departed.Event)
	s.Equal(rewards.LevelGold, departed.Level, "the departure carries their final tier")
	s.Equal("c-001:departed", departed.IdempotencyKey)
	s.Equal("ada@example.com", departed.Email)
}

// A promotion armed in the same instant as soft-deactivate must still be sent
// before the departure notice. The main loop drains needsNotify before
// needsDeparture; without that, departure can win and the promotion is dropped.
func (s *RewardsSuite) Test_Notify_DepartureDrainsAQueuedPromotion() {
	// Asymmetric delays: a slow promotion with an instant departure is the
	// shape that actually needs the drain. If both are slow, the departure
	// round trip can mask a missing drain.
	calls := s.mockNotifyPer(func(r rewards.NotifyRequest) time.Duration {
		if r.Event == rewards.NotifyEventPromoted {
			return 200 * time.Millisecond
		}
		return 0
	})

	// The promotion and soft-deactivate land in the same instant; stopAt later
	// is test-env teardown only.
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.deactivateAt(time.Minute, "leave")
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"a promotion earned in the same instant as soft-deactivate must still be sent")
	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventDeparted),
		"and the departure notice still goes out after it")
}

// NotifiedLevels rides the continue-as-new payload, so the successor run does
// not re-congratulate a customer for a tier they were told about last run.
func (s *RewardsSuite) Test_Notify_NotifiedLevelsSurviveTheRoll() {
	s.mockNotify(0)

	state := newState()
	state.Points = 400
	state.LifetimeEarnEvents = 4

	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.env.ExecuteWorkflow(workflows.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()
	s.Equal([]string{rewards.LevelGold}, next.NotifiedLevels,
		"crossed gold at 500; the successor must know it was announced")
	s.Equal(700, next.Points)
}

// The audit crawl decides what is a notification row by matching the Activity's
// registered name against ActivityNotifyCustomer (internal/httpapi/audit.go),
// and the workflow schedules it by that same string rather than by function
// reference (workflows/notify.go). If the constant and the name the SDK
// registers ever diverge, notification rows simply stop appearing -- no error,
// just a quietly poorer timeline -- and the workflow schedules an Activity no
// worker answers for.
//
// RegisterActivity(&Activities{}) registers every exported method under the
// method's own name, so that is what this checks: that a method with exactly
// this name exists on the struct the worker registers. Renaming the method, or
// unexporting it, fails here.
func TestActivityNameMatchesRegistration(t *testing.T) {
	typ := reflect.TypeOf(testActivities)
	if _, ok := typ.MethodByName(rewards.ActivityNotifyCustomer); !ok {
		var have []string
		for i := 0; i < typ.NumMethod(); i++ {
			have = append(have, typ.Method(i).Name)
		}
		t.Errorf("ActivityNotifyCustomer = %q but %s has no such exported method; "+
			"the SDK would register %v",
			rewards.ActivityNotifyCustomer, typ, have)
	}
}

// mockNotifyFailing fails the first failures deliveries and succeeds after, so a
// send can exhaust its retry budget and a later attempt can still land.
func (s *RewardsSuite) mockNotifyFailing(failures int) *notifyCalls {
	calls := &notifyCalls{}
	var n int
	s.env.OnActivity(testActivities.NotifyCustomer, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req rewards.NotifyRequest) error {
			n++
			if n <= failures {
				return fmt.Errorf("notification provider unreachable (attempt %d)", n)
			}
			calls.add(req)
			return nil
		}).Maybe()
	return calls
}

// Raised on PR #15: a delivery that exhausts its retries was dropped for good.
//
// The Activity's own retry policy is bounded on purpose -- an unbounded one
// would block continue-as-new for as long as the provider stayed down. So the
// outer retry has to come from somewhere else, and "notify a tier the customer
// has reached but not been told about" is that somewhere: any later add picks it
// up, because the condition is a property of the customer rather than an event
// that has already gone past.
//
// Before the fix this failed with no notifications at all: the crossing had
// happened once, so nothing ever re-queued it.
func (s *RewardsSuite) Test_Notify_FailedDeliveryIsRetriedByALaterAdd() {
	// notifyMaxAttempts failures exhausts exactly one send.
	calls := s.mockNotifyFailing(3)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	// Stays gold: no boundary is crossed by this one, which is the whole point.
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"a dropped promotion must be picked up by the next add, not lost for good")
}

// ...and the flip side: re-arming needsNotify on every add must not mean
// announcing the tier on every add. NotifiedLevels is what stops the second
// delivery once the first has succeeded.
func (s *RewardsSuite) Test_Notify_AnnouncesEachTierOnce() {
	calls := s.mockNotifyFailing(0)

	// Three adds, all inside gold after the first.
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.addPoints(3*time.Minute, "u3", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.stopAt(4 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"gold is announced once, however many adds land inside it")
}

// A single add can clear two thresholds at once: MaxPointsPerTxn is 1000 and
// platinum starts at 1000, so one add from zero lands a customer straight in
// platinum without ever being observed at gold.
//
// One notification, naming where they are. Raised on PR #15 as a missed
// promotion; recorded here as a decision instead. Announcing gold and then
// immediately platinum tells the customer something that was true for no
// measurable time, and "Welcome to Gold" arriving beside "Welcome to Platinum"
// reads as a bug to the person receiving it.
//
// It is also not a change: the original crossing rule compared Level(before) to
// Level(after) and likewise produced only platinum. Pinned so that if a later
// phase decides differently, it decides deliberately.
func (s *RewardsSuite) Test_Notify_SingleAddPastTwoTiersAnnouncesOnlyTheNewOne() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{
		Amount: rewards.PlatinumThreshold, Reason: "one big purchase"})
	s.stopAt(2 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelPlatinum}, calls.levels(rewards.NotifyEventPromoted),
		"a customer who never sat at gold should not be congratulated for it")
}

// The boundary of the retry added for PR #15, stated exactly.
//
// A failed delivery is re-offered by later adds *while the customer stays at
// that tier*. Advance a tier first and the lower one is dropped for good, since
// promotionFor only ever offers the tier they are at now.
//
// That is the intended behaviour rather than a gap in it: the customer is
// platinum, and platinum is what they get told. A belated "you reached gold",
// sent after they are already past it, would be worse than silence. Worth
// pinning because the obvious reading of "retried on the next add" is broader
// than what actually happens, and that over-claim is what the first round of
// review on this PR caught in a comment.
func (s *RewardsSuite) Test_Notify_RetryDoesNotSurviveAdvancingATier() {
	calls := s.mockNotifyFailing(3) // exhausts exactly the gold delivery

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "a"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 500, Reason: "b"})
	s.stopAt(3 * time.Minute)

	_ = s.runUntilStopped(newState())

	s.Equal([]string{rewards.LevelPlatinum}, calls.levels(rewards.NotifyEventPromoted),
		"the failed gold notice is dropped; platinum, where they actually are, is sent")
}
