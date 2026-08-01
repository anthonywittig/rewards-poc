// Package workflows holds the customer rewards Entity Workflow and its Update
// and Query handlers. See docs/FINDINGS.md#workflow-design.
//
// Everything here runs under the workflow's determinism constraints. The rules
// it applies live in the parent internal/rewards package as plain functions, so
// this package is orchestration: what to await, what to schedule, when to roll.
//
// It deliberately does not import internal/rewards/activities. The Go SDK has no
// workflow sandbox, so importing the Activity package imports its dependencies,
// and calling one directly from here is a determinism bug the compiler would
// accept. Activities are named by the rewards.ActivityNotifyCustomer constant.
package workflows

import (
	"fmt"
	"strings"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The workflow.GetVersion markers this workflow uses are named in the domain
// package -- rewards.ChangeTierThresholds -- because the API needs the same
// strings to read a run's version back out of TemporalChangeVersion.
// FINDINGS.md#versioning-is-the-real-risk.
//
// The gate that used to live here, changeTierNotifications, is gone. It was
// retired the ordinary way: it had no live runs left on DefaultVersion, so the
// branch it selected was dead code. Removing it is not free of consequences --
// see FINDINGS.md#retiring-a-gate-forfeits-the-histories-it-protected.

// CustomerRewardsWorkflow is one long-lived Entity Workflow per customer.
//
// The main loop is: wait for work, then notify → continue-as-new.
// Product leave is soft (Deactivated flag); the workflow keeps running.
func CustomerRewardsWorkflow(ctx workflow.Context, state rewards.CustomerState) error {
	logger := workflow.GetLogger(ctx)

	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	if err := rewards.ValidateEnrollment(wfID, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", wfID, "error", err)
		return err
	}

	earnsThisRun := 0

	// Armed by addPoints when the customer sits at an unannounced tier.
	needsNotify := false
	// Armed by deactivate so the departure notice is sent outside the Update.
	needsDeparture := false

	if state.EnrolledAt.IsZero() {
		state.EnrolledAt = workflow.Now(ctx)
	}

	// Which tier ladder this run lives by, decided once and never revisited.
	//
	// Lowering the thresholds by TierThresholdDrop moves the balance at which a
	// promotion Activity is scheduled *and* the level every search attribute
	// upsert writes, so it is a command-changing edit twice over. Ungated, a run
	// whose history records "no Activity at 460 points" would replay under the
	// new code and emit a ScheduleActivityTask the history has no event for:
	//
	//	nondeterministic workflow: extra replay command for ScheduleActivityTask
	//
	// Runs recorded before this marker resolve to DefaultVersion and keep the
	// original ladder for the rest of their lives; they move to the cheaper one
	// at their next continue-as-new, at most EarnsPerRun adds away. That means a
	// customer sitting at 460 is basic until their run rolls and gold after --
	// deliberate, and the price of not wedging them.
	//
	// It is resolved before the handlers are registered, on purpose: it must be
	// the same value for every Update the run ever serves, and reading it inside
	// a handler would put a GetVersion marker in the middle of an Update's
	// history rather than at a fixed point.
	tiers := rewards.TiersV1
	if workflow.GetVersion(ctx, rewards.ChangeTierThresholds,
		workflow.DefaultVersion, rewards.VersionTierThresholds) >= rewards.VersionTierThresholds {
		tiers = rewards.TiersV2
	}

	if err := upsertSearchAttributes(ctx, tiers, &state); err != nil {
		return fmt.Errorf("upsert search attributes: %w", err)
	}

	if err := workflow.SetQueryHandler(ctx, rewards.QueryGetStatus, func() (rewards.CustomerStatus, error) {
		return tiers.StatusOf(&state), nil
	}); err != nil {
		return fmt.Errorf("register %s query: %w", rewards.QueryGetStatus, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, rewards.UpdateAddPoints,
		func(ctx workflow.Context, req rewards.AddPointsRequest) (rewards.AddPointsResult, error) {
			if state.Deactivated {
				return rewards.AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					"customer is deactivated; re-enroll them before adding points",
					rewards.ErrTypeDeactivated,
					nil,
				)
			}

			if state.Points+req.Amount > rewards.PointsCap {
				return rewards.AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("add of %d would exceed the cap of %d (balance is %d)",
						req.Amount, rewards.PointsCap, state.Points),
					rewards.ErrTypePointsCapExceeded,
					nil,
				)
			}

			state.Points += req.Amount
			state.LifetimeEarnEvents++

			// Asked of the run's own ladder: on a pre-marker run the same
			// balance that arms this today would not have armed it, and arming
			// it would emit a ScheduleActivityTask its history has no event for.
			if _, ok := tiers.PromotionFor(&state); ok {
				needsNotify = true
				logger.Info("tier promotion pending",
					"customerId", state.CustomerID, "level", tiers.Level(state.Points))
			}

			earnsThisRun++

			eventID := fmt.Sprintf("%s:%d", state.CustomerID, state.LifetimeEarnEvents)

			if err := upsertSearchAttributes(ctx, tiers, &state); err != nil {
				logger.Error("search attribute upsert failed after point add",
					"customerId", state.CustomerID, "error", err)
			}

			logger.Info("points added",
				"customerId", state.CustomerID,
				"amount", req.Amount,
				"reason", req.Reason,
				"balance", state.Points,
				"level", tiers.Level(state.Points),
				"eventId", eventID)

			return rewards.AddPointsResult{
				Balance: state.Points,
				Level:   tiers.Level(state.Points),
				EventID: eventID,
			}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, req rewards.AddPointsRequest) error {
				if req.Amount <= 0 {
					return fmt.Errorf("amount must be positive, got %d", req.Amount)
				}
				if req.Amount > rewards.MaxPointsPerTxn {
					return fmt.Errorf("amount %d exceeds the per-transaction maximum of %d",
						req.Amount, rewards.MaxPointsPerTxn)
				}
				if req.Reason == "" {
					return fmt.Errorf("reason is required")
				}
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateAddPoints, err)
	}

	if err := workflow.SetUpdateHandler(ctx, rewards.UpdateDeactivate,
		func(ctx workflow.Context) (rewards.DeactivateResult, error) {
			if state.Deactivated {
				return rewards.DeactivateResult{Changed: false}, nil
			}

			// Staged on a copy and committed only once the upsert is issued, so
			// a failed Update really did change nothing. Mutating first would
			// leave the customer deactivated and addPoints 409ing while the
			// caller was told it failed.
			//
			// The upsert has to be part of that: if visibility cannot record
			// Active=false the list falls back to ExecutionStatus=Running and
			// shows them active, so a leave visibility never saw is not a leave.
			next := state
			next.Deactivated = true
			if err := upsertSearchAttributes(ctx, tiers, &next); err != nil {
				return rewards.DeactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next
			needsDeparture = true

			logger.Info("customer deactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", tiers.Level(state.Points))
			return rewards.DeactivateResult{Changed: true}, nil
		}); err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateDeactivate, err)
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, rewards.UpdateReactivate,
		func(ctx workflow.Context, req rewards.ReactivateRequest) (rewards.ReactivateResult, error) {
			// Not an error: re-enrolling an active customer is a duplicate, and
			// the API turns Changed=false into a 409. Reported rather than
			// applied, so a racing enroll cannot overwrite a live customer's
			// name and email with a second signup's.
			if !state.Deactivated {
				return rewards.ReactivateResult{Changed: false, Status: tiers.StatusOf(&state)}, nil
			}
			if strings.TrimSpace(req.Email) == "" {
				return rewards.ReactivateResult{}, temporal.NewNonRetryableApplicationError(
					"email is required", rewards.ErrTypeInvalidEnrollment, nil)
			}

			// Staged and committed exactly as deactivate does, and for the same
			// reason: a failed upsert must not leave the customer reactivated
			// under a name and email the caller was told did not take.
			next := state
			if name := strings.TrimSpace(req.Name); name != "" {
				next.Name = name
			}
			next.Email = strings.TrimSpace(req.Email)
			next.Deactivated = false
			if err := upsertSearchAttributes(ctx, tiers, &next); err != nil {
				return rewards.ReactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next

			logger.Info("customer reactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", tiers.Level(state.Points))
			return rewards.ReactivateResult{Changed: true, Status: tiers.StatusOf(&state)}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, req rewards.ReactivateRequest) error {
				if strings.TrimSpace(req.Email) == "" {
					return fmt.Errorf("email is required")
				}
				return nil
			},
		},
	); err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateReactivate, err)
	}

	logger.Info("customer enrolled",
		"customerId", state.CustomerID,
		"generation", state.Generation,
		"points", state.Points,
		"deactivated", state.Deactivated)

	// Production should roll on GetContinueAsNewSuggested() rather than a fixed
	// earn count. FINDINGS.md#continue-as-new.
	for {
		if err := workflow.Await(ctx, func() bool {
			return needsNotify || needsDeparture || earnsThisRun >= rewards.EarnsPerRun
		}); err != nil {
			return err
		}

		if needsNotify {
			needsNotify = false
			deliverPromotion(ctx, tiers, &state)
			continue
		}

		if needsDeparture {
			needsDeparture = false
			if err := sendNotify(ctx, tiers.DepartureNotice(&state)); err != nil {
				logger.Error("departure notification failed after retries",
					"customerId", state.CustomerID, "error", err)
			}
			continue
		}

		if err := workflow.Await(ctx, func() bool {
			return workflow.AllHandlersFinished(ctx)
		}); err != nil {
			return err
		}
		if needsNotify || needsDeparture {
			continue
		}

		state.Generation++

		logger.Info("continuing as new",
			"customerId", state.CustomerID,
			"generation", state.Generation,
			"earnsThisRun", earnsThisRun,
			"points", state.Points)

		return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
	}
}

func upsertSearchAttributes(ctx workflow.Context, tiers rewards.TierLadder, state *rewards.CustomerState) error {
	return workflow.UpsertTypedSearchAttributes(ctx,
		rewards.KeyCustomerID.ValueSet(state.CustomerID),
		rewards.KeyCustomerEmail.ValueSet(state.Email),
		rewards.KeyCustomerName.ValueSet(state.Name),
		rewards.KeyRewardsLevel.ValueSet(tiers.Level(state.Points)),
		rewards.KeyRewardsPoints.ValueSet(int64(state.Points)),
		rewards.KeyEnrolledAt.ValueSet(state.EnrolledAt),
		rewards.KeyGeneration.ValueSet(int64(state.Generation)),
		rewards.KeyActive.ValueSet(!state.Deactivated),
	)
}
