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
// The main loop is: wait for something to do, then depart → notify → continue
// as new, in that order. Departure always wins over a roll; a pending promotion
// is always drained before a roll. The Update handler never awaits the
// notification Activity -- it only arms a flag the loop observes.
//
// It returns a DepartureSummary on the way out, which is why the signature is
// not the bare `error` an entity workflow usually carries: the run closes
// Completed, and a completion has a result payload worth filling in. Every other
// exit -- a rejected enrollment, a rollover -- returns the zero value alongside
// its error, since neither of those is a departure.
func CustomerRewardsWorkflow(ctx workflow.Context, state CustomerState) (DepartureSummary, error) {
	logger := workflow.GetLogger(ctx)

	// Nothing else will catch a bad payload, so this runs before anything is
	// upserted or served. See validateEnrollment.
	if err := validateEnrollment(ctx, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", workflow.GetInfo(ctx).WorkflowExecution.ID, "error", err)
		return DepartureSummary{}, err
	}

	// Successful adds in *this* run, as opposed to state.LifetimeEarnEvents
	// which spans every run. Resets to zero on continue-as-new, by construction:
	// it is a local, not part of the carried state.
	earnsThisRun := 0

	// Armed by addPoints when the customer sits at an unannounced tier. The
	// main loop clears it, delivers (or finds there is nothing left to say),
	// and only re-arms on a later add -- that is the outer retry for a failed
	// delivery. PLAN.md 3.7.
	needsNotify := false

	// EnrolledAt is set once, on the first run, and carried forward from then on.
	// workflow.Now() rather than time.Now() -- PLAN.md 12.5.
	if state.EnrolledAt.IsZero() {
		state.EnrolledAt = workflow.Now(ctx)
	}

	// Re-asserted every run rather than relying on continue-as-new inheritance.
	// One code path establishes the invariant; see PLAN.md 4.
	if err := upsertSearchAttributes(ctx, &state); err != nil {
		return DepartureSummary{}, fmt.Errorf("upsert search attributes: %w", err)
	}

	if err := workflow.SetQueryHandler(ctx, QueryGetStatus, func() (CustomerStatus, error) {
		return statusOf(&state), nil
	}); err != nil {
		return DepartureSummary{}, fmt.Errorf("register %s query: %w", QueryGetStatus, err)
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

			// Arm, never await. An awaited Activity here would couple the
			// point-add to the notifier's availability -- the points are already
			// earned and recorded, so a notification provider being down must
			// not fail them or hold the caller open. PLAN.md 3.7.
			if _, ok := promotionFor(&state); ok {
				needsNotify = true
				logger.Info("tier promotion pending",
					"customerId", state.CustomerID, "level", Level(state.Points))
			}

			// Drives continue-as-new. The handler only counts -- the roll itself
			// happens in the main loop, because continue-as-new is not
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
		return DepartureSummary{}, fmt.Errorf("register %s update: %w", UpdateAddPoints, err)
	}

	logger.Info("customer enrolled",
		"customerId", state.CustomerID,
		"generation", state.Generation,
		"points", state.Points)

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
	for {
		if err := workflow.Await(ctx, func() bool {
			return ctx.Err() != nil || needsNotify || earnsThisRun >= EarnsPerRun
		}); err != nil {
			// Await itself saw the cancel (condition was still false). Same
			// destination as the nil-return trap below.
			return handleLeave(ctx, &state, &needsNotify), nil
		}

		// A nil error above does NOT mean "not cancelled". workflow.Await
		// evaluates its condition before it checks cancellation:
		//
		//	for !condition() {
		//	    ... return NewCanceledError(...) if ctx is done ...
		//	    state.yield("Await")
		//	}
		//	return nil
		//
		// so once the condition holds it returns nil without ever looking at
		// ctx. A cancel arriving in the same workflow transition as the Nth add
		// (or a promotion) therefore lands here with err == nil and ctx already
		// done.
		//
		// Rolling at that point strands the departure permanently:
		// continue-as-new starts a fresh run, and the cancellation request
		// targeted the run that just ended. The customer clicks deactivate and
		// stays active. Departure always wins -- the points are already
		// recorded either way.
		if ctx.Err() != nil {
			return handleLeave(ctx, &state, &needsNotify), nil
		}

		// Drain promotions before rolling. Sending here (not in the Update
		// handler) keeps the point-add off the notification provider's
		// critical path, and doing it in the main loop rather than a
		// workflow.Go goroutine means AllHandlersFinished is enough before a
		// roll -- there is no side goroutine for it to miss (PLAN.md 12.6).
		if needsNotify {
			needsNotify = false
			// Disconnected: a deactivation arriving mid-delivery must not
			// cancel a promotion already earned. handleLeave still runs after
			// if ctx is done, and will send the departure notice.
			dctx, cancel := workflow.NewDisconnectedContext(ctx)
			deliverPromotion(dctx, &state)
			cancel()
			continue
		}

		// Ready to roll. An Update accepted just before the threshold fired
		// may still be running; rolling would abort it, and the caller would
		// get an error for points that were about to be applied.
		// AllHandlersFinished covers that. A promotion armed by such an Update
		// is re-checked after the wait -- continue, don't roll past it.
		if err := workflow.Await(ctx, func() bool {
			return workflow.AllHandlersFinished(ctx)
		}); err != nil || ctx.Err() != nil {
			return handleLeave(ctx, &state, &needsNotify), nil
		}
		if needsNotify {
			continue
		}

		state.Generation++

		logger.Info("continuing as new",
			"customerId", state.CustomerID,
			"generation", state.Generation,
			"earnsThisRun", earnsThisRun,
			"points", state.Points)

		return DepartureSummary{}, workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
	}
}

// handleLeave records a graceful departure and closes the execution as
// Completed, returning the customer's final standing as the workflow result.
//
// Cancel rather than Terminate is what makes this reachable at all: Terminate
// skips workflow code entirely, so none of this would run. PLAN.md 3.6.
//
// Note what this does *not* return: ctx.Err(). Returning the cancellation error
// is the reflex, and it closes the execution as Canceled -- which is what this
// workflow used to do. A customer leaving the program is not an aborted piece of
// work, though; it is the last thing the workflow was for, and it runs to the
// end of its own departure procedure before returning. Completed says that.
// Canceled says the opposite, and reserving it leaves it free to mean what it
// should: something went wrong and the work stopped early.
//
// The cancellation itself is still recorded -- WorkflowExecutionCancelRequested
// is in history whatever we return, and it is the event the audit crawl reads
// for the departure row. So nothing is hidden by not echoing it back; the run
// simply closes on its own terms. PLAN.md 3.6.
func handleLeave(ctx workflow.Context, state *CustomerState, needsNotify *bool) DepartureSummary {
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

	// Drain in-flight Updates first: returning while an addPoints is mid-flight
	// loses its result. A promotion armed by that Update (or earlier) is
	// delivered next -- a customer who reached gold and then left still reached
	// gold.
	if err := workflow.Await(dctx, func() bool {
		return workflow.AllHandlersFinished(dctx)
	}); err != nil {
		logger.Warn("failed waiting for in-flight work to drain on departure", "error", err)
	}

	if *needsNotify {
		*needsNotify = false
		deliverPromotion(dctx, state)
	}

	// The departure notice reuses the promotion Activity rather than adding a
	// second one -- which is why there is no cleanup Activity in this design.
	// Sent synchronously because it is the last thing that happens; there is no
	// run left for a queue to be drained by. PLAN.md 3.7.
	if err := sendNotify(dctx, departureNotice(state)); err != nil {
		logger.Error("departure notification failed after retries",
			"customerId", state.CustomerID, "error", err)
	}

	// workflow.Now on the disconnected context: the cancelled one still reports
	// time perfectly well, but reading anything off ctx here invites the next
	// person to reach for something that does need a live context.
	return DepartureSummary{
		CustomerID:         state.CustomerID,
		DepartedAt:         workflow.Now(dctx),
		FinalPoints:        state.Points,
		FinalLevel:         Level(state.Points),
		LifetimeEarnEvents: state.LifetimeEarnEvents,
		Generation:         state.Generation,
	}
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
