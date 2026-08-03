package workflows

import (
	"fmt"
	"strings"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
)

// validateEnrollment checks a starting payload against the workflow ID it was
// started under. The workflow is the only integrity boundary in this design --
// there is no database schema behind it -- so a bad enrollment is refused here.
//
// Every error is non-retryable: a payload that fails once will fail on every
// attempt.
func validateEnrollment(workflowID string, state *rewards.CustomerState) error {
	if !strings.HasPrefix(workflowID, rewards.WorkflowIDPrefix) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("workflow ID %q does not start with %q", workflowID, rewards.WorkflowIDPrefix),
			rewards.ErrTypeInvalidEnrollment, nil)
	}
	if want := strings.TrimPrefix(workflowID, rewards.WorkflowIDPrefix); state.CustomerID != want {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("customerId %q does not match workflow ID %q (expected customerId %q)",
				state.CustomerID, workflowID, want),
			rewards.ErrTypeInvalidEnrollment, nil)
	}
	if strings.TrimSpace(state.Name) == "" {
		return temporal.NewNonRetryableApplicationError(
			"name is required", rewards.ErrTypeInvalidEnrollment, nil)
	}

	if state.Points < 0 || state.LifetimeEarnEvents < 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("counters must be non-negative (points=%d lifetimeEarnEvents=%d)",
				state.Points, state.LifetimeEarnEvents),
			rewards.ErrTypeInvalidEnrollment, nil)
	}

	// Run numbers are 1-based: the enrollment run is run 1. A zero here means
	// the payload was built by something that never set it.
	if state.RunNumber < 1 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("runNumber must be at least 1 (the enrollment run is run 1), got %d",
				state.RunNumber),
			rewards.ErrTypeInvalidEnrollment, nil)
	}

	if state.Points > rewards.PointsCap {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points (%d) exceeds the cap of %d", state.Points, rewards.PointsCap),
			rewards.ErrTypeInvalidEnrollment, nil)
	}

	if state.LifetimeEarnEvents == 0 && state.Points > 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points is %d but lifetimeEarnEvents is 0", state.Points),
			rewards.ErrTypeInvalidEnrollment, nil)
	}

	return nil
}
