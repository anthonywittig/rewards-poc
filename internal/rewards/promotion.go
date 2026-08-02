package rewards

import "slices"

// What to notify a customer about, decided from state alone. The delivery half
// -- scheduling the Activity, handling its failure -- needs a workflow.Context
// and lives in internal/rewards/workflows.

// PromotionFor returns the notification to send if the customer has reached a
// tier nobody has told them about.
//
// Deliberately *not* "did this add cross a boundary". A crossing happens once
// and is then gone, so a delivery that failed its retries could never be
// attempted again. Asking whether the customer's current tier appears in
// NotifiedLevels makes the condition a property of the customer, so a later add
// retries it. Pinned by Test_Notify_FailedDeliveryIsRetriedByALaterAdd.
//
// Two things it deliberately does not do, both pinned by tests:
//
//   - **It offers only the tier they are at, never the ones they passed.** One
//     add of MaxPointsPerTxn from zero lands straight in platinum, and gold is
//     never announced.
//   - **The retry does not survive a tier advance.** A failed gold notice is
//     re-offered only while the customer is still gold; reach platinum first and
//     gold is dropped for good.
func PromotionFor(state *CustomerState) (NotifyRequest, bool) {
	// Top down, stopping at the first rule the balance satisfies: the highest
	// match wins and the ones below are never reached, which is "only the tier
	// they are at" written as control flow.
	//
	// Announcing every tier passed instead takes two changes, not one. Reversing
	// this walk alone announces the *lowest* unannounced tier -- one add of
	// MaxPointsPerTxn lands a customer in platinum and congratulates them for
	// gold -- because deliverPromotion sends a single notification per drain. It
	// would also need a deliverPromotion that loops until this returns false.
	//
	// Nobody is congratulated for basic: it is not a rung, so falling out of the
	// loop is exactly the basic case.
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
			Event:      NotifyEventPromoted,
			Level:      t.Level,
			// A customer reaches each tier once, so this is a natural key --
			// which is what makes the retry above safe to send more than once.
			IdempotencyKey: state.CustomerID + ":" + t.Level,
		}, true
	}
	return NotifyRequest{}, false
}

// DepartureNotice is the same Activity reused for "this customer left", which is
// why there is no separate cleanup Activity.
// FINDINGS.md#tier-promotion-notifications.
func DepartureNotice(state *CustomerState) NotifyRequest {
	return NotifyRequest{
		CustomerID:     state.CustomerID,
		Event:          NotifyEventDeparted,
		Level:          Level(state.Points),
		IdempotencyKey: state.CustomerID + ":" + NotifyEventDeparted,
	}
}
