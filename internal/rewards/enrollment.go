package rewards

import (
	"fmt"
	"strings"

	"go.temporal.io/sdk/temporal"
)

// ValidateEnrollment checks a starting payload against the workflow ID it was
// started under. The workflow is the only integrity boundary in this design --
// there is no database schema behind it -- so this is where a bad enrollment
// is refused.
//
// Every error is non-retryable: a bad payload will not become good on retry.
func ValidateEnrollment(workflowID string, state *CustomerState) error {
	if !strings.HasPrefix(workflowID, WorkflowIDPrefix) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("workflow ID %q does not start with %q", workflowID, WorkflowIDPrefix),
			ErrTypeInvalidEnrollment, nil)
	}
	if want := strings.TrimPrefix(workflowID, WorkflowIDPrefix); state.CustomerID != want {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("customerId %q does not match workflow ID %q (expected customerId %q)",
				state.CustomerID, workflowID, want),
			ErrTypeInvalidEnrollment, nil)
	}
	if strings.TrimSpace(state.Name) == "" {
		return temporal.NewNonRetryableApplicationError(
			"name is required", ErrTypeInvalidEnrollment, nil)
	}
	if state.Points < 0 || state.Generation < 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("counters must be non-negative (points=%d generation=%d)",
				state.Points, state.Generation),
			ErrTypeInvalidEnrollment, nil)
	}
	if state.Points > PointsCap {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("points (%d) exceeds the cap of %d", state.Points, PointsCap),
			ErrTypeInvalidEnrollment, nil)
	}
	return nil
}
