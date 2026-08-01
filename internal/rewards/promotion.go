package rewards

import "slices"

// What to notify a customer about, decided from state alone. The delivery half
// -- scheduling the Activity, handling its failure -- lives in
// internal/rewards/workflows, because that half needs a workflow.Context and
// this half does not.
//
// Splitting them that way is what lets these rules be tested as plain functions
// rather than through a test environment, and it keeps the tier ladder (which is
// unexported, and only in this package) as the single place a threshold is
// attached to a tier.

// PromotionFor returns the notification to send if the customer has reached a
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
func PromotionFor(state *CustomerState) (NotifyRequest, bool) {
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

// DepartureNotice is the same Activity reused for "this customer left", which is
// why there is no separate cleanup Activity. PLAN.md 3.7.
func DepartureNotice(state *CustomerState) NotifyRequest {
	return NotifyRequest{
		CustomerID:     state.CustomerID,
		Email:          state.Email,
		Event:          NotifyEventDeparted,
		Level:          Level(state.Points),
		IdempotencyKey: state.CustomerID + ":" + NotifyEventDeparted,
	}
}
