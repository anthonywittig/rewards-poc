// Package workflows holds the customer rewards Entity Workflow: one long-lived
// workflow per customer, driven by Updates and read by a Query. The rules it
// applies live in the parent internal/rewards package as plain functions.
//
// There are deliberately no Activities: points, tier, membership and the audit
// trail are all workflow state and Event History, which is the point of the
// POC. A real system's side effects (say, notifying on promotion) belong in
// Activities the workflow schedules by name.
//
// Entity workflows outlive deploys. In production, any edit that changes the
// commands a run emits must be gated with workflow.GetVersion; this POC resets
// executions instead, and the replay test is what catches such an edit.
package workflows

import (
	"fmt"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// CustomerRewardsWorkflow is one long-lived Entity Workflow per customer. Each
// run accepts a handful of point-adds, then continues as new carrying state
// forward. Leaving the program is one-way: deactivate sets the flag and the
// run completes instead of rolling, ending the customer's workflow for good.
func CustomerRewardsWorkflow(ctx workflow.Context, state rewards.CustomerState) error {
	logger := workflow.GetLogger(ctx)

	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	if err := rewards.ValidateEnrollment(wfID, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", wfID, "error", err)
		return err
	}

	earnsThisRun := 0

	if state.EnrolledAt.IsZero() {
		state.EnrolledAt = workflow.Now(ctx)
	}

	if err := upsertSearchAttributes(ctx, &state); err != nil {
		return fmt.Errorf("upsert search attributes: %w", err)
	}

	if err := workflow.SetQueryHandler(ctx, rewards.QueryGetStatus, func() (rewards.CustomerStatus, error) {
		return rewards.StatusOf(&state), nil
	}); err != nil {
		return fmt.Errorf("register %s query: %w", rewards.QueryGetStatus, err)
	}

	// The validator/handler split: a validator rejection writes nothing to
	// Event History, a handler rejection is recorded. Facts about the request
	// (amount, reason) belong in the validator; facts about the customer's
	// accumulated state (the cap) belong in the handler.
	err := workflow.SetUpdateHandlerWithOptions(ctx, rewards.UpdateAddPoints,
		func(ctx workflow.Context, req rewards.AddPointsRequest) (rewards.AddPointsResult, error) {
			// Only reachable in the window between the deactivate committing
			// and this run completing -- afterwards the Update finds no open
			// run at all, and the API answers for it.
			if state.Deactivated {
				return rewards.AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					"customer is deactivated; deactivation is permanent",
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
			earnsThisRun++

			if err := upsertSearchAttributes(ctx, &state); err != nil {
				logger.Error("search attribute upsert failed after point add",
					"customerId", state.CustomerID, "error", err)
			}

			logger.Info("points added",
				"customerId", state.CustomerID,
				"amount", req.Amount,
				"reason", req.Reason,
				"balance", state.Points,
				"level", rewards.Level(state.Points))

			return rewards.AddPointsResult{
				Balance: state.Points,
				Level:   rewards.Level(state.Points),
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

	// Setting the flag is what ends the workflow: the main coroutine below is
	// also awaiting it, and completes the run once every handler has drained.
	if err := workflow.SetUpdateHandler(ctx, rewards.UpdateDeactivate,
		func(ctx workflow.Context) (rewards.DeactivateResult, error) {
			// A concurrent duplicate in the drain window; a repeat DELETE after
			// the run closed never reaches this handler at all.
			if state.Deactivated {
				return rewards.DeactivateResult{Changed: false}, nil
			}

			// Staged on a copy and committed only once the upsert is issued, so
			// a failed Update really did change nothing -- and completion is
			// not reversible.
			next := state
			next.Deactivated = true
			if err := upsertSearchAttributes(ctx, &next); err != nil {
				return rewards.DeactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next

			logger.Info("customer deactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", rewards.Level(state.Points))
			return rewards.DeactivateResult{Changed: true}, nil
		}); err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateDeactivate, err)
	}

	logger.Info("customer enrolled",
		"customerId", state.CustomerID,
		"runNumber", state.RunNumber,
		"points", state.Points,
		"deactivated", state.Deactivated)

	// Continue-as-new after a fixed number of adds, so history per run stays
	// bounded. Production should roll on GetContinueAsNewSuggested() instead.
	if err := workflow.Await(ctx, func() bool {
		return earnsThisRun >= rewards.EarnsPerRun || state.Deactivated
	}); err != nil {
		return err
	}

	// Let any in-flight handler finish before the run closes underneath it.
	if err := workflow.Await(ctx, func() bool {
		return workflow.AllHandlersFinished(ctx)
	}); err != nil {
		return err
	}

	// Checked after the drain, so a deactivate that lands while a due roll
	// waits for handlers still ends the workflow rather than rolling it into a
	// run nothing can ever wake.
	if state.Deactivated {
		logger.Info("customer deactivated; completing the workflow",
			"customerId", state.CustomerID,
			"runNumber", state.RunNumber,
			"points", state.Points)
		return nil
	}

	state.RunNumber++

	logger.Info("continuing as new",
		"customerId", state.CustomerID,
		"runNumber", state.RunNumber,
		"earnsThisRun", earnsThisRun,
		"points", state.Points)

	return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
}

func upsertSearchAttributes(ctx workflow.Context, state *rewards.CustomerState) error {
	return workflow.UpsertTypedSearchAttributes(ctx,
		rewards.KeyCustomerID.ValueSet(state.CustomerID),
		rewards.KeyCustomerName.ValueSet(state.Name),
		rewards.KeyRewardsLevel.ValueSet(rewards.Level(state.Points)),
		rewards.KeyRewardsPoints.ValueSet(int64(state.Points)),
		rewards.KeyEnrolledAt.ValueSet(state.EnrolledAt),
		rewards.KeyRunNumber.ValueSet(int64(state.RunNumber)),
		rewards.KeyActive.ValueSet(!state.Deactivated),
	)
}
