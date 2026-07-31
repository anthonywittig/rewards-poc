package rewards_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

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

func (s *RewardsSuite) SetupTest() { s.env = s.newEnv() }

// newEnv builds a test environment the workflow will actually run in.
//
// Two things every test needs. The workflow validates that its payload's
// customerId matches the workflow ID it was started under, so the env's
// "default-test-workflow-id" has to be replaced with a real one. And since
// Phase 6 the workflow schedules an Activity on departure, so an env with none
// registered fails every cancellation path with "no activity is registered for
// taskqueue 'rewards'" -- which is the test environment being right: the
// workflow does now have a side effect, and pretending otherwise would hide it.
func (s *RewardsSuite) newEnv() *testsuite.TestWorkflowEnvironment {
	env := s.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID:        rewards.WorkflowID(testCustomerID),
		TaskQueue: rewards.TaskQueue,
	})
	env.RegisterActivity(rewards.NotifyCustomer)
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
			// The test env hands back the concrete value the handler returned.
			if res, ok := v.(rewards.AddPointsResult); ok {
				r.value = res
			} else {
				s.Failf("unexpected update result", "got %T, want rewards.AddPointsResult", v)
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

// cancelAt schedules the graceful-departure path. Every test needs one: the
// Phase 1 workflow runs until cancelled, so without this the env would block.
func (s *RewardsSuite) cancelAt(at time.Duration) {
	s.env.RegisterDelayedCallback(func() { s.env.CancelWorkflow() }, at)
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

func (s *RewardsSuite) runToCancellation(state rewards.CustomerState) error {
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)
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

// --- Tier derivation (PLAN.md 3.2) -----------------------------------------

// Pure-function boundaries. Cheap to assert exhaustively, and these are the
// numbers a stakeholder will ask about.
func TestLevelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		points int
		want   string
	}{
		{0, rewards.LevelBasic},
		{1, rewards.LevelBasic},
		{499, rewards.LevelBasic},
		{500, rewards.LevelGold},
		{501, rewards.LevelGold},
		{999, rewards.LevelGold},
		{1000, rewards.LevelPlatinum},
		{50000, rewards.LevelPlatinum},
	} {
		if got := rewards.Level(tc.points); got != tc.want {
			t.Errorf("Level(%d) = %q, want %q", tc.points, got, tc.want)
		}
	}
}

func TestNextTierAt(t *testing.T) {
	for _, tc := range []struct {
		points  int
		wantAt  int
		wantHas bool
	}{
		{0, rewards.GoldThreshold, true},
		{499, rewards.GoldThreshold, true},
		{500, rewards.PlatinumThreshold, true},
		{999, rewards.PlatinumThreshold, true},
		{1000, 0, false},
	} {
		gotAt, gotHas := rewards.NextTierAt(tc.points)
		if gotAt != tc.wantAt || gotHas != tc.wantHas {
			t.Errorf("NextTierAt(%d) = (%d, %v), want (%d, %v)",
				tc.points, gotAt, gotHas, tc.wantAt, tc.wantHas)
		}
	}
}

// --- addPoints happy path ---------------------------------------------------

func (s *RewardsSuite) Test_AddPoints_AppliesAndDerivesTier() {
	add := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "signup bonus"})
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(3 * time.Minute)

	_ = s.runToCancellation(newState())

	s.Equal(rewards.LevelBasic, first.value.Level, "499 points is still basic")
	s.Equal(rewards.LevelGold, second.value.Level, "500 points promotes to gold")
	s.Equal(500, second.value.Balance)
}

func (s *RewardsSuite) Test_AddPoints_AccumulatesLifetimeCounters() {
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 100, Reason: "a"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 250, Reason: "b"})
	status := s.queryStatusAt(3 * time.Minute)
	s.cancelAt(4 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(time.Duration(len(cases)+2) * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(4 * time.Minute)

	_ = s.runToCancellation(state)

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
	s.cancelAt(3 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(3 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(state)

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
			env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)

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

	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)

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
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(state)

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
	s.cancelAt(time.Duration(len(adds)+1) * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, newState())

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()
	s.Equal(1, next.Generation)
}

// One short of the threshold, the run is still going -- so the exit really is
// driven by the counter rather than by anything incidental.
func (s *RewardsSuite) Test_ContinueAsNew_DoesNotFireEarly() {
	for i := 0; i < rewards.EarnsPerRun-1; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	}
	s.cancelAt(time.Duration(rewards.EarnsPerRun+1) * time.Minute)

	err := s.runToCancellation(newState())

	// Cancelled, not continued-as-new.
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
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)

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
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, newState())
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
	env2.RegisterDelayedCallback(env2.CancelWorkflow, time.Duration(rewards.EarnsPerRun+1)*time.Minute)
	env2.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, next)

	s.Require().True(env2.IsWorkflowCompleted())
	var canErr *workflow.ContinueAsNewError
	s.False(errors.As(env2.GetWorkflowError(), &canErr),
		"the successor run must start its own count, not inherit a primed one")
}

// awaitSemanticsWorkflow pins the SDK behaviour the ctx.Err() guards in
// CustomerRewardsWorkflow exist for. It reports (awaitReturnedNil, ctxWasDone)
// for an Await whose condition is already true on an already-cancelled context.
func awaitSemanticsWorkflow(ctx workflow.Context) ([]bool, error) {
	cctx, cancel := workflow.WithCancel(ctx)
	cancel()

	err := workflow.Await(cctx, func() bool { return true })
	return []bool{err == nil, cctx.Err() != nil}, nil
}

// workflow.Await returning nil does NOT mean "not cancelled". It evaluates its
// condition before it checks the context, so a satisfied condition short-
// circuits the cancellation check entirely.
//
// That is the whole reason CustomerRewardsWorkflow re-checks ctx.Err() after
// each Await: on a real server a single workflow task can carry both the Nth
// addPoints and a cancellation request, and without the guard the run would
// roll instead of deactivating -- stranding the departure permanently, since
// continue-as-new starts a fresh run and the cancellation targeted the run that
// just ended.
//
// This asserts the primitive rather than that scenario. The test environment
// dispatches env.UpdateWorkflow synchronously, running the main coroutine to
// quiescence before the next env call, so it cannot stage two things landing in
// one transition -- verified by attempting it. The guards are therefore correct
// by construction but not exercised end to end here; see PLAN.md 12.9.
func (s *RewardsSuite) Test_AwaitReturnsNilOnCancelledContextWhenConditionHolds() {
	s.env.ExecuteWorkflow(awaitSemanticsWorkflow)

	s.Require().True(s.env.IsWorkflowCompleted())
	s.Require().NoError(s.env.GetWorkflowError())

	var got []bool
	s.Require().NoError(s.env.GetWorkflowResult(&got))

	s.True(got[0], "Await returned nil despite the context being cancelled")
	s.True(got[1], "...and the context really was cancelled -- hence the explicit ctx.Err() guards")
}

// Deactivation beats the roll: a cancel arriving before the threshold takes the
// departure path rather than continuing as new.
func (s *RewardsSuite) Test_ContinueAsNew_CancelWinsBeforeThreshold() {
	s.addPoints(time.Minute, "u0", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.cancelAt(2 * time.Minute)

	err := s.runToCancellation(newState())

	var canceled *temporal.CanceledError
	s.True(errors.As(err, &canceled))
}

// --- Cancellation (PLAN.md 3.6) ---------------------------------------------

// Cancel, not Terminate: the workflow's own code runs on the way out, and the
// execution closes as Canceled so the customer list can tell active from
// deactivated via ExecutionStatus.
func (s *RewardsSuite) Test_Cancel_ClosesAsCanceled() {
	s.cancelAt(time.Minute)

	err := s.runToCancellation(newState())

	s.Require().Error(err)
	var canceled *temporal.CanceledError
	s.True(errors.As(err, &canceled), "want a CanceledError, got %T: %v", err, err)
}

// An Update delivered in the same instant as the cancellation still applies,
// and the workflow still closes as Canceled.
//
// Note on what this does *not* cover: handleLeave's drain. The addPoints
// handler never blocks -- it does arithmetic and returns -- so it has always
// finished by the time cancellation is processed, and this test still passes
// with the drain removed. Phase 6 made that guard testable at last, via the one
// thing that genuinely can still be outstanding at departure: see
// Test_Notify_DepartureDrainsAQueuedPromotion.
func (s *RewardsSuite) Test_Cancel_AppliesConcurrentUpdate() {
	add := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 250, Reason: "purchase"})
	s.cancelAt(time.Minute) // same instant as the update

	err := s.runToCancellation(newState())

	s.NoError(add.rejected)
	s.NoError(add.completed, "the concurrent update must still complete")
	s.Equal(250, add.value.Balance)

	var canceled *temporal.CanceledError
	s.True(errors.As(err, &canceled))
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
	s.env.OnActivity(rewards.NotifyCustomer, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req rewards.NotifyRequest) error {
			if d := delay(req); d > 0 {
				time.Sleep(d)
			}
			calls.add(req)
			return nil
		}).Maybe()
	return calls
}

// THE test PLAN.md 10 asks for, and the reason it says to write it first.
//
// A promotion landing on the *third* add is the ordinary case at
// EarnsPerRun = 3, and it is precisely when the run wants to continue as new.
// workflow.AllHandlersFinished returns true the moment the Update handler
// returns -- it knows nothing about the workflow.Go goroutine still holding a
// queued notification (PLAN.md 12.6) -- so without notifier.idle() in the
// pre-roll condition the roll wins and the notification is dropped silently.
//
// Verified to fail before the guard existed:
//
//	--- FAIL: Test_Notify_PromotionOnTheRollingAddIsNotDropped
//	    expected the promotion to survive the roll, got no notifications
func (s *RewardsSuite) Test_Notify_PromotionOnTheRollingAddIsNotDropped() {
	calls := s.mockNotify(50 * time.Millisecond)

	// 200 + 200 + 200 = 600: basic until the third add, gold on it.
	for i := 0; i < rewards.EarnsPerRun; i++ {
		s.addPoints(time.Duration(i+1)*time.Minute, fmt.Sprintf("u%d", i),
			rewards.AddPointsRequest{Amount: 200, Reason: "purchase"})
	}
	// No cancel: the roll is what ends this run.
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, newState())

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
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(newState())

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted))
}

// Both boundaries crossed inside one run produce two notifications, in order.
// Worth pinning because the queue is drained one at a time by a single
// goroutine, so a second promotion arriving while the first is in flight has to
// wait rather than be lost.
func (s *RewardsSuite) Test_Notify_BothTiersInOneRun() {
	calls := s.mockNotify(20 * time.Millisecond)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.cancelAt(3 * time.Minute)

	_ = s.runToCancellation(newState())

	s.Equal([]string{rewards.LevelGold, rewards.LevelPlatinum},
		calls.levels(rewards.NotifyEventPromoted))
}

// An add that stays inside a tier notifies nobody. The obvious case, and the one
// that would make the demo unbearable if it were wrong.
func (s *RewardsSuite) Test_Notify_NoPromotionWithinATier() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 100, Reason: "purchase"})
	s.cancelAt(3 * time.Minute)

	_ = s.runToCancellation(newState())

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
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(state)

	s.Empty(calls.levels(rewards.NotifyEventPromoted),
		"gold was already notified; crossing it again must not re-send")
}

// Departure reuses the same Activity, which is why there is no separate cleanup
// Activity in the design. PLAN.md 3.7.
func (s *RewardsSuite) Test_Notify_DepartureUsesTheSameActivity() {
	calls := s.mockNotify(0)

	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 600, Reason: "purchase"})
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(newState())

	sent := calls.all()
	s.Require().Len(sent, 2, "expected a promotion and a departure, got %v", sent)

	departed := sent[1]
	s.Equal(rewards.NotifyEventDeparted, departed.Event)
	s.Equal(rewards.LevelGold, departed.Level, "the departure carries their final tier")
	s.Equal("c-001:departed", departed.IdempotencyKey)
	s.Equal("ada@example.com", departed.Email)
}

// The departure drain, which until now could not be tested.
//
// PLAN.md 3.5 and the Phase 2 tests both carried a note that handleLeave's drain
// was unfalsifiable: the handler did arithmetic and returned, so it had always
// finished by the time cancellation was processed, and the tests passed with the
// drain removed. A queued notification is the first thing that can genuinely
// still be outstanding when the customer leaves -- so this is the test that
// makes that guard real, and it fails if the notifier clause is dropped from
// handleLeave's await.
func (s *RewardsSuite) Test_Notify_DepartureDrainsAQueuedPromotion() {
	// The delays are asymmetric on purpose. With both slow, the departure's own
	// round trip happens to hold the workflow open long enough for the promotion
	// to land alongside it, and the test passes whether or not the drain exists
	// -- which it did, until a mutation run showed the guard could be deleted
	// with everything still green. A promotion that outlives an instant
	// departure is the only shape that actually needs the drain.
	calls := s.mockNotifyPer(func(r rewards.NotifyRequest) time.Duration {
		if r.Event == rewards.NotifyEventPromoted {
			return 200 * time.Millisecond
		}
		return 0
	})

	// The promotion and the cancellation land in the same instant.
	s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 500, Reason: "purchase"})
	s.cancelAt(time.Minute)

	err := s.runToCancellation(newState())

	var canceled *temporal.CanceledError
	s.True(errors.As(err, &canceled), "still closes as Canceled")

	s.Equal([]string{rewards.LevelGold}, calls.levels(rewards.NotifyEventPromoted),
		"a promotion earned in the same instant as the departure must still be sent")
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
	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	next := s.continuedState()
	s.Equal([]string{rewards.LevelGold}, next.NotifiedLevels,
		"crossed gold at 500; the successor must know it was announced")
	s.Equal(700, next.Points)
}

// The audit crawl decides what is a notification row by matching the Activity's
// registered name against ActivityNotifyCustomer (internal/httpapi/audit.go).
// If the constant and the name the SDK registers ever diverge, notification rows
// simply stop appearing -- no error, just a quietly poorer timeline. Derive the
// registered name the same way the SDK does and compare.
func TestActivityNameMatchesRegistration(t *testing.T) {
	fn := runtime.FuncForPC(reflect.ValueOf(rewards.NotifyCustomer).Pointer()).Name()
	registered := fn
	if i := strings.LastIndex(fn, "."); i >= 0 {
		registered = fn[i+1:]
	}
	if registered != rewards.ActivityNotifyCustomer {
		t.Errorf("ActivityNotifyCustomer = %q but the SDK would register %q (from %q)",
			rewards.ActivityNotifyCustomer, registered, fn)
	}
}
