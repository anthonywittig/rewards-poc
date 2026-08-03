// Package workflows holds the customer rewards Entity Workflow and its Update
// and Query handlers.
//
// Everything here runs under the workflow's determinism constraints. The rules
// it applies live in the parent internal/rewards package as plain functions, so
// this package is orchestration: what to await, when to roll.
//
// There are deliberately no Activities. Nothing in the rewards program needs a
// side effect -- points, tier, membership and the audit trail are all workflow
// state and Event History, which is rather the point of the POC. A real system
// would notify customers on promotion; that is an Activity, and it belongs in a
// sibling internal/rewards/activities package the workflow schedules *by name*,
// never by import -- the Go SDK has no workflow sandbox, so a package boundary
// is the only structural guard keeping provider SDKs out of workflow code.
//
// Entity workflows outlive deploys, so in production any edit that changes the
// commands a run emits must be gated with workflow.GetVersion or it wedges
// every execution already in flight. This POC skips that machinery and resets
// executions instead; the replay test is what would catch such an edit.
package workflows

import (
	"fmt"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// CustomerRewardsWorkflow is one long-lived Entity Workflow per customer.
//
// Each run accepts a handful of point-adds, then continues as new.
// Product leave is one-way: deactivate sets the flag and the run completes
// instead of rolling, ending the customer's workflow for good.
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
			// a failed Update really did change nothing. Mutating first would
			// complete the whole workflow while the caller was told the leave
			// failed -- and completion is not reversible.
			//
			// The upsert has to be part of that: if visibility cannot record
			// Active=false the list falls back to ExecutionStatus=Running and
			// shows them active, so a leave visibility never saw is not a leave.
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
		"generation", state.Generation,
		"points", state.Points,
		"deactivated", state.Deactivated)

	// Production should roll on GetContinueAsNewSuggested() rather than a fixed
	// earn count.
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
			"generation", state.Generation,
			"points", state.Points)
		return nil
	}

	state.Generation++

	logger.Info("continuing as new",
		"customerId", state.CustomerID,
		"generation", state.Generation,
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
		rewards.KeyGeneration.ValueSet(int64(state.Generation)),
		rewards.KeyActive.ValueSet(!state.Deactivated),
	)
}
