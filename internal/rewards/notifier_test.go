package rewards

import "testing"

// Unit tests for the queue itself, in-package because notifier is unexported.
//
// The workflow-level tests cannot reach this: they can only observe what was
// delivered, and a duplicate queue entry for a level that succeeds first time is
// invisible from there -- the second entry is skipped because NotifiedLevels
// already has it, not because the queue refused it. Confirmed by mutation: both
// clauses below could be deleted with the whole suite still green.

func promotion(level string) NotifyRequest {
	return NotifyRequest{Event: NotifyEventPromoted, Level: level}
}

// A level already waiting is not queued again.
//
// This matters because promotionFor re-offers an unannounced tier on *every*
// add (that is the outer retry for a failed delivery). With a provider that is
// down, nothing would ever remove entries from the queue, so without this the
// queue grows by one per add -- and continue-as-new waits for it to drain.
func TestNotifierQueueIsIdempotentPerLevel(t *testing.T) {
	n := &notifier{}

	if !n.queue(promotion(LevelGold)) {
		t.Fatal("the first gold promotion should queue")
	}
	if n.queue(promotion(LevelGold)) {
		t.Error("a second gold promotion must not queue while the first is waiting")
	}
	if len(n.pending) != 1 {
		t.Errorf("pending = %d entries, want 1", len(n.pending))
	}

	// A different tier is a different notification.
	if !n.queue(promotion(LevelPlatinum)) {
		t.Error("platinum is not gold and should queue")
	}
	if len(n.pending) != 2 {
		t.Errorf("pending = %d entries, want 2", len(n.pending))
	}
}

// The delivery in flight counts as queued. It has been taken off pending but is
// not yet in NotifiedLevels, so this is precisely the window an add can land in.
func TestNotifierQueueSkipsTheInFlightLevel(t *testing.T) {
	gold := promotion(LevelGold)
	n := &notifier{current: &gold}

	if n.queue(promotion(LevelGold)) {
		t.Error("gold is being delivered right now; queueing it again would double-send")
	}
	if len(n.pending) != 0 {
		t.Errorf("pending = %d entries, want 0", len(n.pending))
	}

	// ...but the notifier is still not idle, which is what holds off the roll.
	if n.idle() {
		t.Error("a delivery in flight means not idle")
	}
}

// An event and a level together identify a notification: a departure carrying
// the customer's final tier must not be mistaken for a promotion to it.
func TestNotifierQueueDistinguishesEventFromLevel(t *testing.T) {
	n := &notifier{}

	if !n.queue(promotion(LevelGold)) {
		t.Fatal("promotion should queue")
	}
	if !n.queue(NotifyRequest{Event: NotifyEventDeparted, Level: LevelGold}) {
		t.Error("a departure at gold is a different notification from a promotion to gold")
	}
}

// idle() is what both the pre-roll guard and the departure drain wait on.
func TestNotifierIdle(t *testing.T) {
	n := &notifier{}
	if !n.idle() {
		t.Error("a fresh notifier is idle")
	}

	n.queue(promotion(LevelGold))
	if n.idle() {
		t.Error("a queued notification means not idle")
	}
}
