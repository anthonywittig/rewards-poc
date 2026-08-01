// Package workflows holds the customer rewards Entity Workflow and its Update
// and Query handlers. See docs/PLAN.md section 3.
//
// Everything here runs under the workflow's determinism constraints. The rules
// it applies -- tiers, enrollment validity, which promotion is owed -- live in
// the parent internal/rewards package as plain functions, so this package is
// orchestration and little else: what to await, what to schedule, when to roll.
//
// It deliberately does not import internal/rewards/activities. The Go SDK has no
// workflow sandbox, so an import of the Activity package is an import of its
// dependencies, and calling one of those directly from here is a determinism bug
// the compiler would be happy to accept. Activities are named by the
// rewards.ActivityNotifyCustomer string constant instead.
package workflows

import (
	"fmt"
	"strings"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Versioning markers for workflow.GetVersion. PLAN.md 12.11.
//
// Entity workflows outlive deploys by design, so a change that alters the
// commands a run emits has to be gated or it breaks every execution already in
// flight. These names are recorded in Event History and can never be reused or
// renamed -- a marker is as permanent as the history it sits in.
//
// They are unexported and live beside the workflow rather than with the domain
// types, because a version marker means nothing outside the code it gates.
const (
	// changeTierNotifications gates the Phase 6 notification Activity. Runs
	// started before it keep the Phase 5 behaviour for the rest of their lives,
	// and pick notifications up at their next continue-as-new -- at most
	// rewards.EarnsPerRun adds away.
	changeTierNotifications  = "tier-notifications"
	versionTierNotifications = 1
)

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

	if err := upsertSearchAttributes(ctx, &state); err != nil {
		return fmt.Errorf("upsert search attributes: %w", err)
	}

	// Phase 6 added the notification Activity, and adding it is a *breaking*
	// change to every run already in flight: the new code emits a
	// ScheduleActivityTask command where the recorded history has no matching
	// event, so replay fails and the workflow task retries forever. Every
	// existing customer would wedge on deploy.
	//
	// Not a hypothetical -- replay_test.go reproduces it against histories
	// recorded by the Phase 5 worker:
	//
	//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
	//	  (ActivityType:(Name:NotifyCustomer) ...)
	//
	// GetVersion is the fix. Runs whose history predates this marker resolve to
	// DefaultVersion and keep behaving exactly as they did; new runs record
	// version 1 and notify. PLAN.md 12.11.
	//
	// One population it cannot save, and the gate should be honest about it:
	// executions created by the *ungated* Phase 6 build. Their history contains
	// the Activity and no marker, so they resolve to DefaultVersion too, and
	// replay then omits an Activity the history demands. GetVersion cannot tell
	// "predates the change" from "ran the change before it was gated" -- the
	// marker is the only signal, and neither has one. Find them with
	// TemporalChangeVersion IS NULL plus a StartTime lower bound, and reset them.
	// Pinned by TestReplay_UngatedPhase6HistoriesCannotBeRescued.
	//
	// The lesson is upstream: gate a command-changing edit in the same commit
	// that introduces it.
	notifyEnabled := workflow.GetVersion(ctx, changeTierNotifications,
		workflow.DefaultVersion, versionTierNotifications) >= versionTierNotifications

	if err := workflow.SetQueryHandler(ctx, rewards.QueryGetStatus, func() (rewards.CustomerStatus, error) {
		return rewards.StatusOf(&state), nil
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

			// Gated: a run whose history predates the version marker must not arm
			// this, or the loop emits a ScheduleActivityTask the history has no
			// event for. See notifyEnabled above.
			if _, ok := rewards.PromotionFor(&state); ok && notifyEnabled {
				needsNotify = true
				logger.Info("tier promotion pending",
					"customerId", state.CustomerID, "level", rewards.Level(state.Points))
			}

			earnsThisRun++

			eventID := fmt.Sprintf("%s:%d", state.CustomerID, state.LifetimeEarnEvents)

			if err := upsertSearchAttributes(ctx, &state); err != nil {
				logger.Error("search attribute upsert failed after point add",
					"customerId", state.CustomerID, "error", err)
			}

			logger.Info("points added",
				"customerId", state.CustomerID,
				"amount", req.Amount,
				"reason", req.Reason,
				"balance", state.Points,
				"level", rewards.Level(state.Points),
				"eventId", eventID)

			return rewards.AddPointsResult{
				Balance: state.Points,
				Level:   rewards.Level(state.Points),
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

			// Staged on a copy and committed only once the upsert is issued, so a
			// failed Update really did change nothing. Mutating first and returning
			// the error would leave the customer deactivated, the departure notice
			// armed, and addPoints 409ing -- while the caller was told it failed.
			//
			// The upsert has to be part of that: if visibility cannot record
			// Active=false the list falls back to ExecutionStatus=Running and shows
			// them active, so a leave visibility never saw is not a leave.
			next := state
			next.Deactivated = true
			if err := upsertSearchAttributes(ctx, &next); err != nil {
				return rewards.DeactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next
			// Gated for the same reason as the promotion above: the departure notice
			// schedules the same Activity, and a pre-marker run being deactivated
			// after the deploy must not emit a command its history cannot account for.
			needsDeparture = notifyEnabled

			logger.Info("customer deactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", rewards.Level(state.Points))
			return rewards.DeactivateResult{Changed: true}, nil
		}); err != nil {
		return fmt.Errorf("register %s update: %w", rewards.UpdateDeactivate, err)
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, rewards.UpdateReactivate,
		func(ctx workflow.Context, req rewards.ReactivateRequest) (rewards.ReactivateResult, error) {
			// Not an error: re-enrolling an active customer is a duplicate, and
			// the API turns Changed=false into the 409 it owes them. Reported
			// rather than applied, so a racing enroll cannot quietly overwrite a
			// live customer's name and email with a second signup's.
			if !state.Deactivated {
				return rewards.ReactivateResult{Changed: false, Status: rewards.StatusOf(&state)}, nil
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
			if err := upsertSearchAttributes(ctx, &next); err != nil {
				return rewards.ReactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next

			logger.Info("customer reactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", rewards.Level(state.Points))
			return rewards.ReactivateResult{Changed: true, Status: rewards.StatusOf(&state)}, nil
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
	// earn count -- see the longer note that used to live here, and PLAN.md 3.5.
	for {
		if err := workflow.Await(ctx, func() bool {
			return needsNotify || needsDeparture || earnsThisRun >= rewards.EarnsPerRun
		}); err != nil {
			return err
		}

		if needsNotify {
			needsNotify = false
			deliverPromotion(ctx, &state)
			continue
		}

		if needsDeparture {
			needsDeparture = false
			if err := sendNotify(ctx, rewards.DepartureNotice(&state)); err != nil {
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

func upsertSearchAttributes(ctx workflow.Context, state *rewards.CustomerState) error {
	return workflow.UpsertTypedSearchAttributes(ctx,
		rewards.KeyCustomerID.ValueSet(state.CustomerID),
		rewards.KeyCustomerEmail.ValueSet(state.Email),
		rewards.KeyCustomerName.ValueSet(state.Name),
		rewards.KeyRewardsLevel.ValueSet(rewards.Level(state.Points)),
		rewards.KeyRewardsPoints.ValueSet(int64(state.Points)),
		rewards.KeyEnrolledAt.ValueSet(state.EnrolledAt),
		rewards.KeyGeneration.ValueSet(int64(state.Generation)),
		rewards.KeyActive.ValueSet(!state.Deactivated),
	)
}
