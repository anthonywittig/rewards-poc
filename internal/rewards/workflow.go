package rewards

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Handler names. Exported because the API layer (Phase 3) and the `temporal`
// CLI both address handlers by string, and a typo there is a runtime error.
const (
	UpdateAddPoints = "addPoints"
	QueryGetStatus  = "getStatus"
)

// Error types returned by the Update handler. The API layer maps these to HTTP
// status codes in Phase 3; naming them here keeps that mapping from being a
// string match on an error message.
const (
	ErrTypePointsCapExceeded = "PointsCapExceeded"
	ErrTypeInvalidEnrollment = "InvalidEnrollment"
)

// AddPointsRequest is the addPoints Update argument.
type AddPointsRequest struct {
	Amount int    `json:"amount"`
	Reason string `json:"reason"`
}

// AddPointsResult is what a successful add returns to the caller.
type AddPointsResult struct {
	Balance int    `json:"balance"`
	Level   string `json:"level"`
	EventID string `json:"eventId"`
}

// CustomerStatus is the getStatus Query result.
type CustomerStatus struct {
	CustomerID string    `json:"customerId"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Points     int       `json:"points"`
	Level      string    `json:"level"`
	NextTierAt int       `json:"nextTierAt"` // 0 when already at the top tier
	EnrolledAt time.Time `json:"enrolledAt"`

	LifetimeEarnEvents int `json:"lifetimeEarnEvents"`
	Generation         int `json:"generation"`
}

// CustomerRewardsWorkflow is one long-lived Entity Workflow per customer.
//
// Phase 1 shape: the workflow enrolls, serves addPoints and getStatus, and runs
// until cancelled. Continue-as-new (PLAN.md 3.5) lands in Phase 2 and replaces
// the await predicate below; the tier-promotion Activity (PLAN.md 3.7) lands in
// Phase 6.
func CustomerRewardsWorkflow(ctx workflow.Context, state CustomerState) error {
	logger := workflow.GetLogger(ctx)

	// Nothing else will catch a bad payload, so this runs before anything is
	// upserted or served. See validateEnrollment.
	if err := validateEnrollment(ctx, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", workflow.GetInfo(ctx).WorkflowExecution.ID, "error", err)
		return err
	}

	// Successful adds in *this* run, as opposed to state.LifetimeEarnEvents
	// which spans every run. Resets to zero on continue-as-new, by construction:
	// it is a local, not part of the carried state.
	earnsThisRun := 0

	// EnrolledAt is set once, on the first run, and carried forward from then on.
	// workflow.Now() rather than time.Now() -- PLAN.md 12.5.
	if state.EnrolledAt.IsZero() {
		state.EnrolledAt = workflow.Now(ctx)
	}

	// Re-asserted every run rather than relying on continue-as-new inheritance.
	// One code path establishes the invariant; see PLAN.md 4.
	if err := upsertSearchAttributes(ctx, &state); err != nil {
		return fmt.Errorf("upsert search attributes: %w", err)
	}

	// Phase 6 added the notification Activity, and adding it is a *breaking*
	// change to every run already in flight: the new code emits a
	// ScheduleActivityTask command where the recorded history has no matching
	// event, so replay fails and the workflow task retries forever. Every
	// existing customer would wedge on deploy.
	//
	// That is not a hypothetical -- the replay test in replay_test.go reproduces
	// it against histories recorded by the Phase 5 worker:
	//
	//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
	//	  (ActivityType:(Name:NotifyCustomer) ...)
	//
	// GetVersion is the fix. Runs whose history predates this marker resolve to
	// DefaultVersion and keep behaving exactly as they did; new runs record
	// version 1 and notify. PLAN.md 12.11.
	//
	// One population it cannot save, and the gate has to be honest about it:
	// executions created by the *ungated* Phase 6 build. Their history contains
	// the Activity and no marker, so they resolve to DefaultVersion too, and the
	// replay then omits an Activity the history demands. GetVersion cannot tell
	// "predates the change" from "ran the change before it was gated" -- the
	// marker is the only signal, and neither has one.
	//
	// Gating anyway is the right trade: it protects every run recorded before
	// Phase 6, which is everyone, at the cost of those started inside the window
	// between two commits. Find them with TemporalChangeVersion IS NULL plus a
	// StartTime lower bound, and reset them. Raised on PR #16 and pinned by
	// TestReplay_UngatedPhase6HistoriesCannotBeRescued.
	//
	// The lesson is upstream: gate a command-changing edit in the same commit
	// that introduces it.
	//
	// Called unconditionally and before anything reads it, because the marker's
	// position in history is itself part of the replayable sequence.
	notifyEnabled := workflow.GetVersion(ctx, changeTierNotifications,
		workflow.DefaultVersion, versionTierNotifications) >= versionTierNotifications

	// The tier-promotion notifier (PLAN.md 3.7). It runs on a disconnected
	// context so that a deactivation cannot cancel a promotion that was already
	// earned; handleLeave waits for it rather than racing it.
	n := &notifier{}
	if notifyEnabled {
		nctx, stopNotifier := workflow.NewDisconnectedContext(ctx)
		defer stopNotifier()
		workflow.Go(nctx, func(gctx workflow.Context) { n.run(gctx, &state) })
	}

	if err := workflow.SetQueryHandler(ctx, QueryGetStatus, func() (CustomerStatus, error) {
		return statusOf(&state), nil
	}); err != nil {
		return fmt.Errorf("register %s query: %w", QueryGetStatus, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddPoints,
		func(ctx workflow.Context, req AddPointsRequest) (AddPointsResult, error) {
			// Reached only after the validator passed, so the request shape is
			// already known good. What is left is the one rule that depends on
			// accumulated state -- and that a support rep would want to see a
			// record of. PLAN.md 3.4.
			if state.Points+req.Amount > PointsCap {
				// Non-retryable: the answer will not change on a retry, and the
				// default retryable flag shows up in the CLI and API responses
				// as an invitation to try again.
				return AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("add of %d would exceed the cap of %d (balance is %d)",
						req.Amount, PointsCap, state.Points),
					ErrTypePointsCapExceeded,
					nil,
				)
			}

			// The only mutation of Points in the system, and it only ever adds.
			state.Points += req.Amount
			state.LifetimeEarnEvents++

			// Queue, never await. An awaited Activity here would couple the
			// point-add to the notifier's availability -- the points are already
			// earned and recorded, so a notification provider being down must
			// not fail them or hold the caller open. PLAN.md 3.7.
			if notifyEnabled {
				if note, ok := promotionFor(&state); ok && n.queue(note) {
					logger.Info("tier promotion queued",
						"customerId", state.CustomerID, "level", note.Level)
				}
			}

			// Drives continue-as-new. The handler only counts -- the roll itself
			// happens in the main function, because continue-as-new is not
			// supported inside an Update handler. PLAN.md 3.5.
			earnsThisRun++

			// Deterministic and stable across replay, unlike a UUID. Lifetime
			// event count is monotonic across continue-as-new, so this stays
			// unique for the life of the customer.
			eventID := fmt.Sprintf("%s:%d", state.CustomerID, state.LifetimeEarnEvents)

			if err := upsertSearchAttributes(ctx, &state); err != nil {
				// The points are already applied and will be recorded in history
				// regardless. Failing the update here would tell the caller the
				// add did not happen, which would be a lie.
				logger.Error("search attribute upsert failed after point add",
					"customerId", state.CustomerID, "error", err)
			}

			logger.Info("points added",
				"customerId", state.CustomerID,
				"amount", req.Amount,
				"reason", req.Reason,
				"balance", state.Points,
				"level", Level(state.Points),
				"eventId", eventID)

			return AddPointsResult{
				Balance: state.Points,
				Level:   Level(state.Points),
				EventID: eventID,
			}, nil
		},
		workflow.UpdateHandlerOptions{
			// Facts about the request, not about the customer. Rejections here
			// write nothing at all to Event History -- no trace, no audit row,
			// no history growth from a client retry loop. PLAN.md 3.4.
			Validator: func(ctx workflow.Context, req AddPointsRequest) error {
				if req.Amount <= 0 {
					return fmt.Errorf("amount must be positive, got %d", req.Amount)
				}
				if req.Amount > MaxPointsPerTxn {
					return fmt.Errorf("amount %d exceeds the per-transaction maximum of %d",
						req.Amount, MaxPointsPerTxn)
				}
				if req.Reason == "" {
					return fmt.Errorf("reason is required")
				}
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateAddPoints, err)
	}

	logger.Info("customer enrolled",
		"customerId", state.CustomerID,
		"generation", state.Generation,
		"points", state.Points)

	// Run until it is time to roll over, or until the customer leaves.
	//
	// A FIXED COUNT IS THE WRONG RULE FOR PRODUCTION. It is used here because
	// three adds is easy to demonstrate, not because it is defensible: what
	// actually matters is history *size*, and a fixed count is only a proxy for
	// it. Three adds is wastefully early for a customer whose updates are small,
	// and would be far too late if each add carried a large payload -- the
	// limits are 50k events / 50 MB per run, and neither is a count of adds.
	//
	// The server already tracks the real thing and will say so:
	//
	//	workflow.GetInfo(ctx).GetContinueAsNewSuggested()
	//
	// which flips to true as a run approaches those limits, and
	// GetContinueAsNewSuggestedReasons() says which one. Production code should
	// roll on that and let the server decide, rather than picking a number.
	// Doing so also sidesteps the versioning hazard on EarnsPerRun: there is no
	// constant to change, so nothing to break running workflows with.
	if err := workflow.Await(ctx, func() bool { return earnsThisRun >= EarnsPerRun }); err != nil {
		return handleLeave(ctx, &state, n, notifyEnabled)
	}

	// A nil error above does NOT mean "not cancelled". workflow.Await evaluates
	// its condition before it checks cancellation:
	//
	//	for !condition() {
	//	    ... return NewCanceledError(...) if ctx is done ...
	//	    state.yield("Await")
	//	}
	//	return nil
	//
	// so once the condition holds it returns nil without ever looking at ctx. A
	// cancel arriving in the same workflow transition as the Nth add therefore
	// lands here with err == nil and ctx already done.
	//
	// Rolling at that point strands the departure permanently: continue-as-new
	// starts a fresh run, and the cancellation request targeted the run that
	// just ended. The customer clicks deactivate and stays active. Departure
	// always wins -- the points are already recorded either way.
	if ctx.Err() != nil {
		return handleLeave(ctx, &state, n, notifyEnabled)
	}

	// Nothing may be left half-done when the run ends. Two separate things can
	// be, and only one of them is the SDK's business:
	//
	//  1. An Update accepted just before the roll condition fired is still
	//     running. Rolling would abort it, and the caller would get an error for
	//     points that were about to be applied. AllHandlersFinished covers this.
	//
	//  2. A promotion notification is queued or in flight in the notifier
	//     goroutine. **AllHandlersFinished does not cover workflow.Go
	//     goroutines** -- it tracks Update and Signal handlers only -- so this
	//     needs its own clause. PLAN.md 12.6.
	//
	// The second half is not defensive coding. A promotion landing on the third
	// add is the ordinary case at EarnsPerRun = 3, and it is exactly when the
	// run wants to roll. Without idle() the notification is dropped in silence:
	// no error, no retry, no trace in history, and a customer who reached gold
	// is never told. Test_Notify_PromotionOnTheRollingAddIsNotDropped fails
	// without it, and was written before it existed -- PLAN.md 10.
	if err := workflow.Await(ctx, func() bool {
		return workflow.AllHandlersFinished(ctx) && n.idle()
	}); err != nil {
		return handleLeave(ctx, &state, n, notifyEnabled)
	}

	// Same trap as above, and a wider window: handlers are usually already
	// finished, so this Await frequently returns nil on its first condition
	// check without ever consulting ctx.
	if ctx.Err() != nil {
		return handleLeave(ctx, &state, n, notifyEnabled)
	}

	state.Generation++

	logger.Info("continuing as new",
		"customerId", state.CustomerID,
		"generation", state.Generation,
		"earnsThisRun", earnsThisRun,
		"points", state.Points)

	return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
}

// handleLeave records a graceful departure and closes the execution as Canceled.
//
// Cancel rather than Terminate is what makes this reachable at all: Terminate
// skips workflow code entirely, so none of this would run. PLAN.md 3.6.
func handleLeave(ctx workflow.Context, state *CustomerState, n *notifier, notifyEnabled bool) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("customer leaving rewards program",
		"customerId", state.CustomerID,
		"finalPoints", state.Points,
		"finalLevel", Level(state.Points),
		"lifetimeEarnEvents", state.LifetimeEarnEvents)

	// ctx is already cancelled, so anything that needs to actually run here has
	// to use a disconnected context.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	// Drain both kinds of in-flight work, for the same reasons as the pre-roll
	// guard above: returning while an addPoints update is mid-flight loses its
	// result, and returning while a promotion is queued drops it. A customer who
	// reached gold and then left still reached gold.
	if err := workflow.Await(dctx, func() bool {
		return workflow.AllHandlersFinished(dctx) && n.idle()
	}); err != nil {
		logger.Warn("failed waiting for in-flight work to drain on departure", "error", err)
	}

	// The departure notice reuses the promotion Activity rather than adding a
	// second one -- which is why there is no cleanup Activity in this design.
	// Sent synchronously because it is the last thing that happens; there is no
	// run left for a queue to be drained by. PLAN.md 3.7.
	if notifyEnabled {
		if err := n.send(dctx, departureNotice(state)); err != nil {
			logger.Error("departure notification failed after retries",
				"customerId", state.CustomerID, "error", err)
		}
	}

	// Returning the cancellation error is what closes the execution as Canceled
	// rather than Completed, which is the distinction the customer list reads to
	// tell active from deactivated. PLAN.md 4.
	return ctx.Err()
}

// validateEnrollment rejects a start payload that is internally inconsistent or
// that disagrees with the workflow ID it was started under.
//
// This matters more here than it would in a conventional service. With no
// application database there is no schema, no CHECK constraint and no unique
// index sitting behind this workflow -- if the start payload is nonsense,
// nothing downstream will notice. "Temporal as the system of record" means the
// workflow *is* the integrity boundary, and the boundary has to be written by
// hand. Everything below would otherwise be enforceable by a table definition.
//
// The state carried across continue-as-new (Phase 2) always satisfies these,
// since we produced it, so this is safe to run at the top of every run rather
// than only on the first.
func validateEnrollment(ctx workflow.Context, state *CustomerState) error {
	// The workflow ID is the real identity -- it is what every operation
	// addresses and what the Phase 5 audit crawl walks. A payload claiming a
	// different customer would index search attributes and answer getStatus
	// under one ID while being addressed by another.
	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	if !strings.HasPrefix(wfID, WorkflowIDPrefix) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("workflow ID %q does not start with %q", wfID, WorkflowIDPrefix),
			ErrTypeInvalidEnrollment, nil)
	}
	if want := strings.TrimPrefix(wfID, WorkflowIDPrefix); state.CustomerID != want {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("customerId %q does not match workflow ID %q (expected customerId %q)",
				state.CustomerID, wfID, want),
			ErrTypeInvalidEnrollment, nil)
	}
	if state.CustomerID == "" {
		return temporal.NewNonRetryableApplicationError(
			"customerId is required", ErrTypeInvalidEnrollment, nil)
	}

	if state.Points < 0 || state.LifetimeEarnEvents < 0 || state.Generation < 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("counters must be non-negative (points=%d lifetimeEarnEvents=%d generation=%d)",
				state.Points, state.LifetimeEarnEvents, state.Generation),
			ErrTypeInvalidEnrollment, nil)
	}

	// The same cap the handler enforces, applied to the starting balance, so it
	// cannot be stepped over on the way in. Collapsing Points and LifetimePoints
	// into one monotonic field is what makes this sufficient: there is no longer
	// a second, caller-supplied number for the cap to be measured against.
	if state.Points > PointsCap {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points (%d) exceeds the cap of %d", state.Points, PointsCap),
			ErrTypeInvalidEnrollment, nil)
	}

	// Points cannot have been earned without an event to earn them in.
	if state.LifetimeEarnEvents == 0 && state.Points > 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points is %d but lifetimeEarnEvents is 0", state.Points),
			ErrTypeInvalidEnrollment, nil)
	}

	return nil
}

func statusOf(state *CustomerState) CustomerStatus {
	nextAt, _ := NextTierAt(state.Points) // 0 when already platinum, which is what we want on the wire
	return CustomerStatus{
		CustomerID:         state.CustomerID,
		Name:               state.Name,
		Email:              state.Email,
		Points:             state.Points,
		Level:              Level(state.Points),
		NextTierAt:         nextAt,
		EnrolledAt:         state.EnrolledAt,
		LifetimeEarnEvents: state.LifetimeEarnEvents,
		Generation:         state.Generation,
	}
}

func upsertSearchAttributes(ctx workflow.Context, state *CustomerState) error {
	return workflow.UpsertTypedSearchAttributes(ctx,
		KeyCustomerID.ValueSet(state.CustomerID),
		KeyCustomerEmail.ValueSet(state.Email),
		KeyCustomerName.ValueSet(state.Name),
		KeyRewardsLevel.ValueSet(Level(state.Points)),
		KeyRewardsPoints.ValueSet(int64(state.Points)),
		KeyEnrolledAt.ValueSet(state.EnrolledAt),
		KeyGeneration.ValueSet(int64(state.Generation)),
	)
}
