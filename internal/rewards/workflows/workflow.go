// Package workflows holds the customer rewards Entity Workflow: one long-lived
// workflow per customer, driven by Updates and read by a Query. The tier rules
// and limits it applies live in the parent internal/rewards package as plain
// functions; enrollment validation lives here, since only the workflow runs it.
//
// There are deliberately no Activities: points, tier, membership and the audit
// trail are all workflow state and Event History, which is the point of the
// POC. A real system's side effects (say, notifying on promotion) belong in
// Activities the workflow schedules by name.
//
// Entity workflows outlive deploys. In production, any edit that changes the
// commands a run emits must be gated with workflow.GetVersion; this POC resets
// executions instead (make destroy && make up).
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
	if err := validateEnrollment(wfID, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", wfID, "error", err)
		return err
	}

	earnsThisRun := 0

	if state.EnrolledAt.IsZero() {
		state.EnrolledAt = workflow.Now(ctx)
	}

	upsertSearchAttributes(ctx, &state)

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

			upsertSearchAttributes(ctx, &state)

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
		func(ctx workflow.Context) error {
			state.Active = false
			upsertSearchAttributes(ctx, &state)

			logger.Info("customer deactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", rewards.Level(state.Points))
			return nil
		}); err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateDeactivate, err)
	}

	logger.Info("customer enrolled",
		"customerId", state.CustomerID,
		"runNumber", state.RunNumber,
		"points", state.Points,
		"active", state.Active)

	// Continue-as-new after a fixed number of adds, so history per run stays
	// bounded. Production should roll on GetContinueAsNewSuggested() instead.
	if err := workflow.Await(ctx, func() bool {
		return earnsThisRun >= rewards.EarnsPerRun || !state.Active
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
	if !state.Active {
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

// upsertSearchAttributes stages the upsert command that ships with the current
// workflow task. The only reachable errors are local programming bugs (an
// unserializable value or a reserved key); server-side rejection surfaces
// later as a workflow task failure, never here. So a bug panics -- failing the
// workflow task, which blocks the run until a fixed worker deploys -- rather
// than failing the entity workflow and ending the customer's record for good.
func upsertSearchAttributes(ctx workflow.Context, state *rewards.CustomerState) {
	err := workflow.UpsertTypedSearchAttributes(ctx,
		rewards.KeyCustomerID.ValueSet(state.CustomerID),
		rewards.KeyCustomerName.ValueSet(state.Name),
		rewards.KeyRewardsLevel.ValueSet(rewards.Level(state.Points)),
		rewards.KeyRewardsPoints.ValueSet(int64(state.Points)),
		rewards.KeyEnrolledAt.ValueSet(state.EnrolledAt),
		rewards.KeyActive.ValueSet(state.Active),
		rewards.KeyRunNumber.ValueSet(int64(state.RunNumber)),
	)
	if err != nil {
		panic(fmt.Errorf("upsert search attributes: %w", err))
	}
}
