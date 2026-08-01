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
	UpdateAddPoints  = "addPoints"
	UpdateDeactivate = "deactivate"
	UpdateReactivate = "reactivate"
	QueryGetStatus   = "getStatus"
)

// Error types returned by Update handlers. The API layer maps these to HTTP
// status codes; naming them here keeps that mapping from being a string match
// on an error message.
const (
	ErrTypePointsCapExceeded = "PointsCapExceeded"
	ErrTypeInvalidEnrollment = "InvalidEnrollment"
	ErrTypeDeactivated       = "Deactivated"
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

// DeactivateResult is what deactivate returns so the audit crawl can tell a
// real leave from an idempotent no-op (repeat DELETE).
type DeactivateResult struct {
	Changed bool `json:"changed"`
}

// ReactivateRequest is the reactivate Update argument (re-enrollment).
type ReactivateRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ReactivateResult mirrors DeactivateResult: Changed distinguishes a real
// re-enrollment from a no-op against a customer who was already active. The API
// needs that to tell a restore (200) from a duplicate enrollment (409), and the
// audit crawl needs it so a no-op does not draw a rejoin row.
type ReactivateResult struct {
	Changed bool           `json:"changed"`
	Status  CustomerStatus `json:"status"`
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

	LifetimeEarnEvents int  `json:"lifetimeEarnEvents"`
	Generation         int  `json:"generation"`
	Active             bool `json:"active"`
}

// CustomerRewardsWorkflow is one long-lived Entity Workflow per customer.
//
// The main loop is: wait for work, then notify → continue-as-new.
// Product leave is soft (Deactivated flag); the workflow keeps running.
func CustomerRewardsWorkflow(ctx workflow.Context, state CustomerState) error {
	logger := workflow.GetLogger(ctx)

	if err := validateEnrollment(ctx, &state); err != nil {
		logger.Error("rejecting enrollment", "workflowId", workflow.GetInfo(ctx).WorkflowExecution.ID, "error", err)
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
	//
	// Called unconditionally and in a fixed position, because the marker's place
	// in history is part of the replayable sequence -- moving this call relative
	// to the other commands is as breaking as removing it.
	notifyEnabled := workflow.GetVersion(ctx, changeTierNotifications,
		workflow.DefaultVersion, versionTierNotifications) >= versionTierNotifications

	if err := workflow.SetQueryHandler(ctx, QueryGetStatus, func() (CustomerStatus, error) {
		return statusOf(&state), nil
	}); err != nil {
		return fmt.Errorf("register %s query: %w", QueryGetStatus, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddPoints,
		func(ctx workflow.Context, req AddPointsRequest) (AddPointsResult, error) {
			if state.Deactivated {
				return AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					"customer is deactivated; re-enroll them before adding points",
					ErrTypeDeactivated,
					nil,
				)
			}

			if state.Points+req.Amount > PointsCap {
				return AddPointsResult{}, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("add of %d would exceed the cap of %d (balance is %d)",
						req.Amount, PointsCap, state.Points),
					ErrTypePointsCapExceeded,
					nil,
				)
			}

			state.Points += req.Amount
			state.LifetimeEarnEvents++

			// Gated: a run whose history predates the version marker must not arm
			// this, or the loop emits a ScheduleActivityTask the history has no
			// event for. See notifyEnabled above.
			if _, ok := promotionFor(&state); ok && notifyEnabled {
				needsNotify = true
				logger.Info("tier promotion pending",
					"customerId", state.CustomerID, "level", Level(state.Points))
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
				"level", Level(state.Points),
				"eventId", eventID)

			return AddPointsResult{
				Balance: state.Points,
				Level:   Level(state.Points),
				EventID: eventID,
			}, nil
		},
		workflow.UpdateHandlerOptions{
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

	if err := workflow.SetUpdateHandler(ctx, UpdateDeactivate, func(ctx workflow.Context) (DeactivateResult, error) {
		if state.Deactivated {
			return DeactivateResult{Changed: false}, nil
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
			return DeactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
		}
		state = next
		// Gated for the same reason as the promotion above: the departure notice
		// schedules the same Activity, and a pre-marker run being deactivated
		// after the deploy must not emit a command its history cannot account for.
		needsDeparture = notifyEnabled

		logger.Info("customer deactivated",
			"customerId", state.CustomerID,
			"points", state.Points,
			"level", Level(state.Points))
		return DeactivateResult{Changed: true}, nil
	}); err != nil {
		return fmt.Errorf("register %s update: %w", UpdateDeactivate, err)
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateReactivate,
		func(ctx workflow.Context, req ReactivateRequest) (ReactivateResult, error) {
			// Not an error: re-enrolling an active customer is a duplicate, and
			// the API turns Changed=false into the 409 it owes them. Reported
			// rather than applied, so a racing enroll cannot quietly overwrite a
			// live customer's name and email with a second signup's.
			if !state.Deactivated {
				return ReactivateResult{Changed: false, Status: statusOf(&state)}, nil
			}
			if strings.TrimSpace(req.Email) == "" {
				return ReactivateResult{}, temporal.NewNonRetryableApplicationError(
					"email is required", ErrTypeInvalidEnrollment, nil)
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
				return ReactivateResult{}, fmt.Errorf("upsert search attributes: %w", err)
			}
			state = next

			logger.Info("customer reactivated",
				"customerId", state.CustomerID,
				"points", state.Points,
				"level", Level(state.Points))
			return ReactivateResult{Changed: true, Status: statusOf(&state)}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, req ReactivateRequest) error {
				if strings.TrimSpace(req.Email) == "" {
					return fmt.Errorf("email is required")
				}
				return nil
			},
		},
	); err != nil {
		return fmt.Errorf("register %s update: %w", UpdateReactivate, err)
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
			return needsNotify || needsDeparture || earnsThisRun >= EarnsPerRun
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
			if err := sendNotify(ctx, departureNotice(&state)); err != nil {
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

func validateEnrollment(ctx workflow.Context, state *CustomerState) error {
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

	if state.Points > PointsCap {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points (%d) exceeds the cap of %d", state.Points, PointsCap),
			ErrTypeInvalidEnrollment, nil)
	}

	if state.LifetimeEarnEvents == 0 && state.Points > 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points is %d but lifetimeEarnEvents is 0", state.Points),
			ErrTypeInvalidEnrollment, nil)
	}

	return nil
}

func statusOf(state *CustomerState) CustomerStatus {
	nextAt, _ := NextTierAt(state.Points)
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
		Active:             !state.Deactivated,
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
		KeyActive.ValueSet(!state.Deactivated),
	)
}
