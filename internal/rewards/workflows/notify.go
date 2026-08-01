package workflows

import (
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Notification delivery. FINDINGS.md#tier-promotion-notifications.
//
// The Update handler does not await the Activity: it applies the points, sets a
// flag, and returns, so a slow or down provider cannot fail a point-add already
// recorded in history or put a network call on the UI's critical path. The main
// loop observes the flag and runs the Activity there.

// notifyTimeout bounds one delivery attempt, and notifyMaxAttempts bounds the
// retries.
//
// Bounding the retries is load-bearing: the default policy retries forever, so
// an unreachable provider would pin the main loop in a retry spin that also
// blocks continue-as-new for as long as the outage lasts.
const (
	notifyTimeout     = 10 * time.Second
	notifyMaxAttempts = 3
)

// deliverPromotion runs NotifyCustomer for the customer's current unannounced
// tier, if any, and records success in NotifiedLevels. The ladder is the run's
// own, so a pre-marker run announces gold at 500 and not at 450.
//
// Callers run this from the workflow main loop so the Update handler never
// awaits the Activity.
func deliverPromotion(ctx workflow.Context, tiers rewards.TierLadder, state *rewards.CustomerState) {
	note, ok := tiers.PromotionFor(state)
	if !ok {
		return
	}

	logger := workflow.GetLogger(ctx)
	if err := sendNotify(ctx, note); err != nil {
		// The Activity's own retries are exhausted by now. Failing the workflow
		// over an undelivered notification would be worse than the
		// notification, so the attempt is abandoned but not given up on: the
		// level stays out of NotifiedLevels, so the next add re-arms the main
		// loop. That is the outer retry rewards.PromotionFor exists to allow.
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
// TestActivityNameMatchesRegistration asserts the name against what the SDK
// actually registers.
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
