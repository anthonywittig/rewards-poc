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

// notifier holds the queue and the delivery currently in flight.
type notifier struct {
	pending []NotifyRequest
	current *NotifyRequest // nil when nothing is being delivered
}

// idle reports whether there is nothing queued and nothing in flight.
//
// This is the whole reason the type exists. workflow.AllHandlersFinished covers
// Update and Signal handlers and says nothing about workflow.Go goroutines
// (PLAN.md 12.6), so without idle() in the pre-roll condition the run continues
// as new while a notification is still queued or executing, and it is silently
// dropped. At EarnsPerRun = 3 a promotion landing on the third add is exactly
// when that happens, which is common rather than exotic.
func (n *notifier) idle() bool { return len(n.pending) == 0 && n.current == nil }

// queue adds a notification for the drain goroutine to pick up, and reports
// whether it did.
//
// Idempotent per (event, level). Since a promotion is now re-queued by any add
// that finds the customer's tier unannounced (see promotionFor), a provider that
// is down would otherwise accumulate one identical entry per add -- and because
// continue-as-new waits for the queue to drain, that would delay the roll
// without bound. Which is the exact failure that bounding the Activity's retries
// was meant to prevent, reintroduced one level up.
func (n *notifier) queue(req NotifyRequest) bool {
	if n.alreadyHas(req) {
		return false
	}
	n.pending = append(n.pending, req)
	return true
}

// alreadyHas reports whether an equivalent notification is queued or in flight.
func (n *notifier) alreadyHas(req NotifyRequest) bool {
	if n.current != nil && n.current.Event == req.Event && n.current.Level == req.Level {
		return true
	}
	for _, p := range n.pending {
		if p.Event == req.Event && p.Level == req.Level {
			return true
		}
	}
	return false
}

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
		n.current = &req

		if err := n.send(ctx, req); err != nil {
			// The Activity's own retries are exhausted by this point, and they
			// are bounded on purpose. Failing the workflow over an undelivered
			// stub notification would be worse than the notification, so this
			// attempt is recorded and abandoned -- but *not* given up on: the
			// level stays out of NotifiedLevels, so the next add re-queues it.
			// That is the outer retry, and it is why promotionFor asks whether
			// the customer's tier has been announced rather than whether this
			// particular add crossed a line.
			logger.Error("notification delivery failed after retries; will retry on the next add",
				"customerId", req.CustomerID, "event", req.Event,
				"level", req.Level, "error", err)
		} else if req.Event == NotifyEventPromoted {
			// Recorded only on success, and only for promotions. Marking a level
			// notified that was never delivered would suppress exactly the retry
			// described above.
			state.NotifiedLevels = append(state.NotifiedLevels, req.Level)
		}

		n.current = nil
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
	current := Level(state.Points)
	// Nobody is congratulated for the tier they started in. Under the old
	// crossing rule this was implicit -- basic can never be crossed into -- and
	// under this one it has to be said.
	if current == LevelBasic {
		return NotifyRequest{}, false
	}
	if slices.Contains(state.NotifiedLevels, current) {
		return NotifyRequest{}, false
	}
	return NotifyRequest{
		CustomerID: state.CustomerID,
		Email:      state.Email,
		Event:      NotifyEventPromoted,
		Level:      current,
		// A customer reaches each tier once, so this is a natural key -- and it
		// is what makes the outer retry above safe to send more than once. The
		// stub ignores it; a real provider would not.
		IdempotencyKey: state.CustomerID + ":" + current,
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
