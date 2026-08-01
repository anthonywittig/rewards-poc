package workflows

import (
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Notification delivery. PLAN.md 3.7.
//
// The half that decides *what* to send -- rewards.PromotionFor,
// rewards.DepartureNotice -- is plain domain logic and lives in the parent
// package. This is the half that needs a workflow.Context: scheduling the
// Activity, bounding its retries, and deciding what a failed send means.
//
// The Update handler does not await the Activity. It applies the points, notices
// an unannounced tier, sets a flag, and returns -- so a notification provider
// being slow or down cannot fail a point-add that has already been recorded in
// history, and cannot put a network call on the UI's critical path. The workflow
// main loop observes the flag and runs the Activity there.

// notifyTimeout bounds one delivery attempt, and notifyMaxAttempts bounds the
// retries.
//
// Bounding the retries is load-bearing rather than tidiness. The default policy
// retries forever, and a failed send is only re-offered on a later add -- so an
// unreachable provider must not pin the main loop in a retry spin that also
// blocks continue-as-new for as long as the outage lasts.
const (
	notifyTimeout     = 10 * time.Second
	notifyMaxAttempts = 3
)

// deliverPromotion runs NotifyCustomer for the customer's current unannounced
// tier, if any, and records success in NotifiedLevels.
//
// Callers run this from the workflow main loop so the Update handler never
// awaits the Activity.
func deliverPromotion(ctx workflow.Context, state *rewards.CustomerState) {
	note, ok := rewards.PromotionFor(state)
	if !ok {
		return
	}

	logger := workflow.GetLogger(ctx)
	if err := sendNotify(ctx, note); err != nil {
		// The Activity's own retries are exhausted by this point, and they are
		// bounded on purpose. Failing the workflow over an undelivered stub
		// notification would be worse than the notification, so this attempt is
		// recorded and abandoned -- but *not* given up on: the level stays out
		// of NotifiedLevels, so the next add re-arms the main loop. That is the
		// outer retry, and it is why rewards.PromotionFor asks whether the
		// customer's tier has been announced rather than whether this particular
		// add crossed a line.
		logger.Error("notification delivery failed after retries; will retry on the next add",
			"customerId", note.CustomerID, "event", note.Event,
			"level", note.Level, "error", err)
		return
	}

	// Recorded only on success, and only for promotions. Marking a level
	// notified that was never delivered would suppress exactly the retry
	// described above.
	state.NotifiedLevels = append(state.NotifiedLevels, note.Level)
}

// sendNotify runs the Activity and waits for it.
//
// Scheduled by name rather than by function reference, which is what keeps this
// package from importing internal/rewards/activities -- see the package doc.
// rewards.ActivityNotifyCustomer is asserted against the method the SDK actually
// registers by TestActivityNameMatchesRegistration.
func sendNotify(ctx workflow.Context, req rewards.NotifyRequest) error {
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: notifyTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumAttempts: notifyMaxAttempts,
		},
	})
	return workflow.ExecuteActivity(actCtx, rewards.ActivityNotifyCustomer, req).Get(ctx, nil)
}
