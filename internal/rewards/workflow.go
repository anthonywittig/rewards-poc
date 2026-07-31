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

	// Run until it is time to roll over, or until the customer leaves. Both
	// exits come out of this Await: a nil error means the roll condition fired,
	// a non-nil error means cancellation.
	if err := workflow.Await(ctx, func() bool { return shouldRoll(ctx, earnsThisRun) }); err != nil {
		return handleLeave(ctx, &state)
	}

	// An Update accepted just before the roll condition fired is still running.
	// Rolling now would abort it -- the caller gets an error for points that
	// were about to be applied. Wait for handlers to drain first. PLAN.md 3.5.
	//
	// Two honest caveats:
	//
	//  1. This is currently unfalsifiable. The handler does arithmetic and
	//     returns without ever blocking, so it has always finished by the time
	//     this is evaluated -- the tests still pass with this Await removed
	//     (verified by mutation). It is here because Phase 6 makes it real, not
	//     because Phase 2 proves it.
	//  2. It covers Update and Signal handlers only. The workflow.Go goroutine
	//     Phase 6 adds is NOT covered by AllHandlersFinished and needs its own
	//     drain clause alongside this one, or its notification is silently
	//     dropped by the roll. PLAN.md 3.7 and 12.6 -- write that test first.
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return handleLeave(ctx, &state)
	}

	state.Generation++

	logger.Info("continuing as new",
		"customerId", state.CustomerID,
		"generation", state.Generation,
		"earnsThisRun", earnsThisRun,
		"points", state.Points)

	return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
}

// shouldRoll reports whether this run has done enough and should hand off to a
// fresh one.
//
// Two modes, so both behaviours are demonstrable (PLAN.md 3.5):
//
//   - EarnsPerRun > 0 -- roll after exactly that many successful adds.
//     Artificially low in this POC so the rollover is easy to watch.
//   - EarnsPerRun == 0 -- defer to the server, which decides based on actual
//     history size. This is what production code should do.
func shouldRoll(ctx workflow.Context, earnsThisRun int) bool {
	if n := EarnsPerRun(); n > 0 {
		return earnsThisRun >= n
	}
	return workflow.GetInfo(ctx).GetContinueAsNewSuggested()
}

// handleLeave records a graceful departure and closes the execution as Canceled.
//
// Cancel rather than Terminate is what makes this reachable at all: Terminate
// skips workflow code entirely, so none of this would run. PLAN.md 3.6.
func handleLeave(ctx workflow.Context, state *CustomerState) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("customer leaving rewards program",
		"customerId", state.CustomerID,
		"finalPoints", state.Points,
		"finalLevel", Level(state.Points),
		"lifetimeEarnEvents", state.LifetimeEarnEvents)

	// ctx is already cancelled, so anything that needs to actually run here has
	// to use a disconnected context. Draining in-flight handlers is the one
	// thing we always do: returning while an addPoints update is mid-flight
	// loses its result and logs an unfinished-handler warning.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	if err := workflow.Await(dctx, func() bool { return workflow.AllHandlersFinished(dctx) }); err != nil {
		logger.Warn("failed waiting for handlers to drain on departure", "error", err)
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
