package rewards

import (
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Tier-promotion delivery helpers. PLAN.md 3.7.
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
func deliverPromotion(ctx workflow.Context, state *CustomerState) {
	note, ok := promotionFor(state)
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
		// outer retry, and it is why promotionFor asks whether the customer's
		// tier has been announced rather than whether this particular add
		// crossed a line.
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
func sendNotify(ctx workflow.Context, req NotifyRequest) error {
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: notifyTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumAttempts: notifyMaxAttempts,
		},
	})
	return workflow.ExecuteActivity(actCtx, ActivityNotifyCustomer, req).Get(ctx, nil)
}

// promotionFor returns the notification to send if the customer has reached a
// tier nobody has told them about.
//
// Deliberately *not* "did this add cross a boundary". A crossing is an event
// that happens once and is then gone, so a delivery that failed its retries
// could never be attempted again -- raised on PR #15, and reproduced by
// Test_Notify_FailedDeliveryIsRetriedByALaterAdd. Asking instead whether the
// customer's current tier appears in NotifiedLevels makes the condition a
// property of the customer, so a later add retries it. The state is already
// carried across continue-as-new, so this costs nothing new.
//
// Two things it deliberately does not do, both pinned by tests:
//
//   - **It offers only the tier they are at, never the ones they passed.** One
//     add of MaxPointsPerTxn from zero lands straight in platinum, and gold is
//     never announced. Announcing both would tell the customer something that
//     was true for no measurable time, and the original crossing rule behaved
//     the same way.
//   - **The retry does not survive a tier advance.** A failed gold notice is
//     re-offered by later adds only while the customer is still gold; reach
//     platinum first and gold is dropped for good. That is the right outcome --
//     they are told where they are -- but it makes "retried on the next add"
//     narrower than it sounds.
//
// It also makes NotifiedLevels genuinely load-bearing in both directions: dedup
// on the way in, and the at-least-once guard PLAN.md 3.7 asks for on the way
// out. Previously the monotonic balance did the deduplication and this was
// belt-and-braces.
//
// Tiers are derived rather than stored (PLAN.md 3.2), so this needs no extra
// state to work out.
func promotionFor(state *CustomerState) (NotifyRequest, bool) {
	// Walk the ladder from the top down and stop at the first rule the balance
	// satisfies. Top-down is the "only the tier they are at" decision above
	// written as control flow rather than left implicit in a Level() call: the
	// highest rule that matches wins, and the ones below it are never reached.
	//
	// Deciding the other way takes two changes rather than one, which is worth
	// stating because the first is the obvious one and is actively harmful on its
	// own. Reversing the walk alone announces the *lowest* unannounced tier: one
	// add of MaxPointsPerTxn lands a customer in platinum and congratulates them
	// for gold, which is worse than either policy. deliverPromotion sends a single
	// notification per drain, so announcing every tier passed needs the reversed
	// walk *and* a deliverPromotion that loops until this returns false.
	//
	// Measured rather than reasoned: the flip alone yields [gold], the flip plus
	// the loop yields [gold platinum], and both fail exactly
	// Test_Notify_SingleAddPastTwoTiersAnnouncesOnlyTheNewOne and
	// Test_Notify_RetryDoesNotSurviveAdvancingATier, which are the two tests that
	// would have to be rewritten to decide the other way.
	//
	// Nobody is congratulated for basic, which needs no clause here: basic is not
	// a rung, so falling out of the loop is exactly the basic case.
	for i := len(tiers) - 1; i >= 0; i-- {
		t := tiers[i]
		if state.Points < t.MinPoints {
			continue
		}
		if slices.Contains(state.NotifiedLevels, t.Level) {
			return NotifyRequest{}, false
		}
		return NotifyRequest{
			CustomerID: state.CustomerID,
			Email:      state.Email,
			Event:      NotifyEventPromoted,
			Level:      t.Level,
			// A customer reaches each tier once, so this is a natural key -- and
			// it is what makes the outer retry above safe to send more than once.
			// The stub ignores it; a real provider would not.
			IdempotencyKey: state.CustomerID + ":" + t.Level,
		}, true
	}
	return NotifyRequest{}, false
}

// departureNotice is the same Activity reused for "this customer left", which is
// why there is no separate cleanup Activity. PLAN.md 3.7.
func departureNotice(state *CustomerState) NotifyRequest {
	return NotifyRequest{
		CustomerID:     state.CustomerID,
		Email:          state.Email,
		Event:          NotifyEventDeparted,
		Level:          Level(state.Points),
		IdempotencyKey: state.CustomerID + ":" + NotifyEventDeparted,
	}
}
