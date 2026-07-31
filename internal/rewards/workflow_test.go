package rewards_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestRewardsSuite(t *testing.T) { suite.Run(t, new(RewardsSuite)) }

type RewardsSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

const testCustomerID = "c-001"

func (s *RewardsSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()
	// The workflow validates that its payload's customerId matches the workflow
	// ID it was started under, so the test env's "default-test-workflow-id" has
	// to be replaced with a real one.
	s.env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID:        rewards.WorkflowID(testCustomerID),
		TaskQueue: rewards.TaskQueue,
	})
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
	s.Equal(350, status.LifetimePoints)
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

// The lifetime cap is enforced in the handler, so unlike the validator cases
// this attempt is accepted, runs, and its failure is recorded in history --
// which is the point: a support rep asking "why didn't they reach platinum?"
// gets an answer.
func (s *RewardsSuite) Test_AddPoints_HandlerRejectsOverLifetimeCap() {
	state := newState()
	state.LifetimePoints = rewards.LifetimePointsCap - 10
	state.Points = 40
	state.LifetimeEarnEvents = 7

	over := s.addPoints(time.Minute, "u1", rewards.AddPointsRequest{Amount: 11, Reason: "purchase"})
	under := s.addPoints(2*time.Minute, "u2", rewards.AddPointsRequest{Amount: 10, Reason: "purchase"})
	status := s.queryStatusAt(3 * time.Minute)
	s.cancelAt(4 * time.Minute)

	_ = s.runToCancellation(state)

	// Accepted by the validator, then failed by the handler.
	s.NoError(over.rejected, "the lifetime cap must not be enforced in the validator")
	s.Require().Error(over.completed)

	var appErr *temporal.ApplicationError
	s.Require().True(errors.As(over.completed, &appErr), "want a typed ApplicationError for the API layer to map")
	s.Equal(rewards.ErrTypeLifetimeCapExceeded, appErr.Type())

	// Landing exactly on the cap is allowed.
	s.NoError(under.completed)

	// The rejected add applied nothing; only the successful one counted.
	s.Equal(50, status.Points)
	s.Equal(rewards.LifetimePointsCap, status.LifetimePoints)
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
		{"balance exceeds lifetime earnings",
			func(st *rewards.CustomerState) { st.Points = 999999; st.LifetimePoints = 0 },
			"cannot exceed lifetimePoints"},
		{"seeded above the lifetime cap",
			func(st *rewards.CustomerState) {
				st.LifetimePoints = rewards.LifetimePointsCap + 1
				st.LifetimeEarnEvents = 1
			},
			"exceeds the lifetime cap"},
		{"negative points",
			func(st *rewards.CustomerState) { st.Points = -1 },
			"non-negative"},
		{"negative generation",
			func(st *rewards.CustomerState) { st.Generation = -1 },
			"non-negative"},
		{"points earned with no earn events",
			func(st *rewards.CustomerState) { st.Points = 10; st.LifetimePoints = 10 },
			"lifetimeEarnEvents is 0"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			env := s.NewTestWorkflowEnvironment()
			env.SetStartWorkflowOptions(client.StartWorkflowOptions{
				ID:        rewards.WorkflowID(testCustomerID),
				TaskQueue: rewards.TaskQueue,
			})

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

// The specific bypass: seed a large balance with lifetimePoints at zero, and
// the handler's cap check has nothing to push against. Rejected at the door.
func (s *RewardsSuite) Test_Enroll_RejectsLifetimeCapBypass() {
	state := newState()
	state.Points = 5_000_000
	state.LifetimePoints = 0

	s.env.ExecuteWorkflow(rewards.CustomerRewardsWorkflow, state)

	s.Require().True(s.env.IsWorkflowCompleted())
	s.Require().Error(s.env.GetWorkflowError())
}

// A balance *below* lifetime earnings is legitimate -- that is what spending
// would produce, and fixtures depend on it -- so it must still be accepted.
func (s *RewardsSuite) Test_Enroll_AcceptsSpentDownBalance() {
	state := newState()
	state.Points = 40
	state.LifetimePoints = 900
	state.LifetimeEarnEvents = 6

	status := s.queryStatusAt(time.Minute)
	s.cancelAt(2 * time.Minute)

	_ = s.runToCancellation(state)

	s.Equal(40, status.Points)
	s.Equal(900, status.LifetimePoints)
	s.Equal(rewards.LevelBasic, status.Level, "tier follows the current balance, not lifetime earnings")
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
// Note on what this does *not* cover: handleLeave drains handlers on a
// disconnected context before returning, and that drain is not exercised here.
// The Phase 1 handler never blocks -- it does arithmetic and returns -- so it
// has always finished by the time cancellation is processed, and the test still
// passes with the drain removed (verified by mutation). The drain only becomes
// load-bearing in Phase 6, when the handler queues work for an Activity that
// can genuinely still be in flight; PLAN.md 10 calls for writing that test
// before the fix. It is kept here because removing and re-adding it later is
// pure churn, not because this test proves it.
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
