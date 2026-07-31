package rewards

import (
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The tier-promotion notifier. PLAN.md 3.7.
//
// The Update handler does not await the Activity. It applies the points,
// notices the tier crossing, queues a notification and returns -- so a
// notification provider being slow or down cannot fail a point-add that has
// already been recorded in history, and cannot put a network call on the UI's
// critical path. Draining the queue is this file's job.

// notifyTimeout bounds one delivery attempt, and notifyMaxAttempts bounds the
// retries.
//
// Bounding the retries is load-bearing rather than tidiness. The default policy
// retries forever, and the continue-as-new guard waits for the notifier to go
// idle -- so an unreachable notification provider would stop the customer's
// workflow rolling for as long as it stayed down, turning a cosmetic outage into
// a stuck entity workflow.
const (
	notifyTimeout     = 10 * time.Second
	notifyMaxAttempts = 3
)

// notifier holds the queue and the two facts the continue-as-new guard needs.
type notifier struct {
	pending  []NotifyRequest
	inFlight bool
}

// idle reports whether there is nothing queued and nothing in flight.
//
// This is the whole reason the type exists. workflow.AllHandlersFinished covers
// Update and Signal handlers and says nothing about workflow.Go goroutines
// (PLAN.md 12.6), so without idle() in the pre-roll condition the run continues
// as new while a notification is still queued or executing, and it is silently
// dropped. At EarnsPerRun = 3 a promotion landing on the third add is exactly
// when that happens, which is common rather than exotic.
func (n *notifier) idle() bool { return len(n.pending) == 0 && !n.inFlight }

// queue adds a notification for the drain goroutine to pick up.
func (n *notifier) queue(req NotifyRequest) { n.pending = append(n.pending, req) }

// run drains the queue until the workflow ends.
//
// Started on a *disconnected* context, so a deactivation arriving while a
// promotion is being delivered does not cancel the delivery. The customer
// genuinely earned the tier; that they left immediately afterwards does not
// unmake it, and handleLeave waits for this to finish before departing.
func (n *notifier) run(ctx workflow.Context, state *CustomerState) {
	logger := workflow.GetLogger(ctx)
	for {
		if err := workflow.Await(ctx, func() bool { return len(n.pending) > 0 }); err != nil {
			return // the workflow is going away
		}

		// Dequeue and mark in-flight with no yield in between, so idle() is
		// never transiently true while a notification is outstanding. A yield
		// here would reopen exactly the hole idle() exists to close.
		req := n.pending[0]
		n.pending = n.pending[1:]
		n.inFlight = true

		if err := n.send(ctx, req); err != nil {
			// Retries are already exhausted by this point. Losing a stub
			// notification is not worth failing the workflow over, and there is
			// nothing useful left to try -- so it is recorded and dropped.
			logger.Error("notification delivery failed after retries",
				"customerId", req.CustomerID, "event", req.Event,
				"level", req.Level, "error", err)
		} else if req.Event == NotifyEventPromoted {
			// Recorded only on success, and only for promotions: this is the
			// dedup guard, and marking a level notified that was never
			// delivered would suppress the retry it exists to allow.
			state.NotifiedLevels = append(state.NotifiedLevels, req.Level)
		}

		n.inFlight = false
	}
}

// send runs the Activity and waits for it.
func (n *notifier) send(ctx workflow.Context, req NotifyRequest) error {
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: notifyTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumAttempts: notifyMaxAttempts,
		},
	})
	return workflow.ExecuteActivity(actCtx, ActivityNotifyCustomer, req).Get(ctx, nil)
}

// promotionFor returns the notification to send if an add crossed a tier
// boundary, given the balance before it.
//
// Tiers are derived rather than stored (PLAN.md 3.2), so detecting a promotion
// is a comparison of two pure function calls and needs no extra state.
func promotionFor(state *CustomerState, pointsBefore int) (NotifyRequest, bool) {
	after := Level(state.Points)
	if Level(pointsBefore) == after {
		return NotifyRequest{}, false
	}
	// The at-least-once guard from PLAN.md 3.7. With points monotonic a level
	// can only be crossed once, so this is currently belt-and-braces -- but it
	// is the check that would start mattering the day a spend or expiry path
	// let a balance fall back below a threshold and climb it again.
	if slices.Contains(state.NotifiedLevels, after) {
		return NotifyRequest{}, false
	}
	return NotifyRequest{
		CustomerID: state.CustomerID,
		Email:      state.Email,
		Event:      NotifyEventPromoted,
		Level:      after,
		// A customer reaches each tier once, so this is a natural key. The stub
		// ignores it; a real provider would not.
		IdempotencyKey: state.CustomerID + ":" + after,
	}, true
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
