package workflows_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/internalbindings"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Replay tests. FINDINGS.md#versioning-and-replay.
//
// A customer's workflow *will* outlive a deploy. A worker picking up a task for
// a run started weeks ago replays that run's recorded history through today's
// code and requires the commands to match event for event; if they do not, the
// workflow task fails and retries forever and the customer is silently wedged.
//
// The fixtures are real histories from three eras:
//
//	pre-notification-*.json             before the notification Activity existed
//	pre-marker-deactivated.json         likewise, and then deactivated
//	ungated-notification.json           from the build that shipped the Activity ungated
//	run-versioned-notification.json     from under the retired tier-notifications gate
//	pre-thresholds-basic-at-460.json    a pre-marker run parked on the original ladder
//	gated-thresholds-gold-at-460.json   the same balance under the current one
//
// Which of them still replay changed when the notification gate was retired, and
// the tests below are that ledger. The last two are the pair that pins the gate
// that replaced it.

// replayCase is one recorded run.
type replayCase struct {
	file string
	what string
}

// The runs the retired tier-notifications gate used to protect.
var preNotificationRuns = []replayCase{
	{"pre-notification-enrollment.json", "first run: enrollment, three adds, then the roll"},
	{"pre-notification-continued.json", "middle run: rolled into, three adds, rolled out of"},
	{"pre-notification-deactivated.json", "final run: one add, then cancelled"},
	{"pre-notification-rejection.json", "a run carrying a handler rejection at the cap"},
	{"pre-marker-deactivated.json", "a marker-free run crossing gold, then deactivated"},
}

func loadHistory(t *testing.T, name string) *historypb.History {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var h historypb.History
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &h); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return &h
}

// replay runs one history through the current workflow code.
//
// OriginalExecution is not optional here, whatever the SDK's doc comment says.
// Without it the replayer invents the workflow ID "ReplayId", validateEnrollment
// refuses a payload whose customerId does not match, and the workflow returns
// before emitting a single command -- which surfaces as a nondeterminism error
// naming whatever event came first. See TestReplay_NeedsTheRecordedWorkflowID.
func replay(t *testing.T, h *historypb.History) error {
	t.Helper()
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflow(workflows.CustomerRewardsWorkflow)

	started := h.GetEvents()[0].GetWorkflowExecutionStartedEventAttributes()
	if started == nil {
		t.Fatal("history does not begin with WorkflowExecutionStarted")
	}
	return r.ReplayWorkflowHistoryWithOptions(nil, h, worker.ReplayWorkflowHistoryOptions{
		OriginalExecution: internalbindings.WorkflowExecution{ID: started.GetWorkflowId()},
	})
}

// hasVersionMarker reports whether the history records any GetVersion decision.
func hasVersionMarker(h *historypb.History) bool {
	for _, e := range h.GetEvents() {
		if m := e.GetMarkerRecordedEventAttributes(); m != nil && m.GetMarkerName() == "Version" {
			return true
		}
	}
	return false
}

// hasNotifyActivity reports whether the history schedules the notification.
func hasNotifyActivity(h *historypb.History) bool {
	for _, e := range h.GetEvents() {
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil &&
			a.GetActivityType().GetName() == rewards.ActivityNotifyCustomer {
			return true
		}
	}
	return false
}

// A run that recorded the *retired* gate still replays with the GetVersion call
// deleted, which is what makes retiring a gate a normal thing to do.
//
// run-versioned-notification.json carries a `Version` marker for a change ID no
// code asks about any more, plus the automatic TemporalChangeVersion upsert that
// came with it (FINDINGS.md#getversion-writes-two-events). Today's workflow
// replays neither as a command, and neither has to be: the SDK matches commands
// to events for Activities, timers and the like, and lets an orphan marker pass.
func TestReplay_HistoriesCarryingTheRetiredMarkerStillReplay(t *testing.T) {
	h := loadHistory(t, "run-versioned-notification.json")

	// The fixture only proves anything if it really carries the retired marker
	// and the Activity that gate controlled.
	if !hasVersionMarker(h) {
		t.Fatal("fixture has no Version marker; it cannot show a retired gate replaying")
	}
	if !hasNotifyActivity(h) {
		t.Fatal("fixture schedules no NotifyCustomer activity; recapture it from a run that crosses a tier")
	}

	if err := replay(t, h); err != nil {
		t.Fatalf("a run recorded under the retired gate no longer replays: %v", err)
	}
}

// The population the retired gate could never save, saved by retiring it.
//
// ungated-notification.json is real: an execution the *ungated* build created,
// with the notification Activity in its history and no marker. Under the gate it
// resolved to DefaultVersion, replay omitted an Activity the history demanded,
// and it was unrescuable. With the gate gone the Activity is scheduled
// unconditionally, the history matches, and the run is well again.
//
// The symmetry is the lesson: a gate protects the runs recorded before it and
// strands the ones recorded beside it, and retiring it swaps which group is
// which. Neither is escapable after the fact — only by not shipping the ungated
// commit in the first place.
//
// It also exercises the *new* gate's DefaultVersion branch in passing: the
// history has no TemporalChangeVersion, so today's code resolves
// rewards.ChangeTierThresholds to DefaultVersion and replays it on
// rewards.TiersV1 throughout. It does not discriminate the two ladders -- its
// balances step 200/400/600, which is gold under both. That is what
// TestReplay_TierThresholdsGate is for.
func TestReplay_UngatedHistoriesReplayNowTheGateIsRetired(t *testing.T) {
	h := loadHistory(t, "ungated-notification.json")

	// Confirm the fixture is the thing this test claims: an Activity, no marker.
	if hasVersionMarker(h) || !hasNotifyActivity(h) {
		t.Fatalf("fixture is not an ungated history (marker=%v activity=%v)",
			hasVersionMarker(h), hasNotifyActivity(h))
	}

	if err := replay(t, h); err != nil {
		t.Fatalf("the ungated histories still cannot be rescued: %v", err)
	}
}

// The bill for retiring the gate, itemised rather than quietly written off.
//
// These histories predate the notification Activity and were the entire reason
// the tier-notifications gate was written. With the gate gone, today's code
// emits a ScheduleActivityTask where they have no event:
//
//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
//	  (ActivityType:(Name:NotifyCustomer) ...)
//
// Accepted deliberately: there is nothing left in flight worth migrating, so the
// answer for any survivor is `make reset`. Asserted rather than deleted, because
// the difference between "these runs are written off" and "we forgot about these
// runs" is exactly this test — and because the query that decides whether a gate
// is safe to retire is worth keeping next to the histories that prove it:
//
//	WorkflowType = 'CustomerRewardsWorkflow'
//	  AND ExecutionStatus = 'Running'
//	  AND TemporalChangeVersion IS NULL
func TestReplay_RetiringTheGateForfeitsTheHistoriesItProtected(t *testing.T) {
	for _, tc := range preNotificationRuns {
		t.Run(tc.file, func(t *testing.T) {
			h := loadHistory(t, tc.file)
			if hasVersionMarker(h) || hasNotifyActivity(h) {
				t.Fatalf("%s is not a pre-notification history (marker=%v activity=%v)",
					tc.file, hasVersionMarker(h), hasNotifyActivity(h))
			}

			if err := replay(t, h); err == nil {
				t.Errorf("%s (%s) replays again -- the notification gate has come "+
					"back, or the Activity has; update this test and "+
					"FINDINGS.md#retiring-a-gate-forfeits-what-it-protected",
					tc.file, tc.what)
			}
		})
	}
}

// The threshold gate, rehearsed on real histories rather than argued from the
// code.
//
// Both fixtures are recorded runs sitting on exactly 460 points -- inside the
// band the version bump moved, above GoldThresholdV2 and below GoldThreshold --
// so they are the same customer under two ladders and the only thing separating
// them is the marker:
//
//	pre-thresholds-basic-at-460.json   no marker, no Activity, RewardsLevel basic
//	gated-thresholds-gold-at-460.json  tier-thresholds-1, NotifyCustomer, gold
//
// The pre-marker one was recorded by a worker built with the GetVersion call
// removed, which is what a run enrolled before the deploy looks like; the
// replayer sees recorded events, not how they were made. It is the fixture that
// actually rehearses this deploy: replaying it through today's code has to
// resolve DefaultVersion, walk TiersV1, find 460 is basic, and emit no Activity.
// Ungate the ladder and it fails the way the whole exercise is about:
//
//	nondeterministic workflow: extra replay command for ScheduleActivityTask:
//	  (ActivityType:(Name:NotifyCustomer) ...)
func TestReplay_TierThresholdsGate(t *testing.T) {
	t.Run("pre-marker run stays on the original ladder", func(t *testing.T) {
		h := loadHistory(t, "pre-thresholds-basic-at-460.json")

		// A marker or an Activity here would mean the fixture was recorded by a
		// gated worker after all, and the test would pass for the wrong reason.
		if hasVersionMarker(h) || hasNotifyActivity(h) {
			t.Fatalf("fixture is not a pre-marker run (marker=%v activity=%v); "+
				"recapture it from a worker built without the GetVersion call",
				hasVersionMarker(h), hasNotifyActivity(h))
		}
		if got := finalPoints(t, h); got != preMarkerBalance {
			t.Fatalf("fixture ends at %d points, not %d -- it is outside the band "+
				"the version bump moved, so it no longer discriminates the ladders", got, preMarkerBalance)
		}

		if err := replay(t, h); err != nil {
			t.Errorf("a customer enrolled before the threshold change is wedged by it: %v", err)
		}
	})

	t.Run("marked run is on the lowered ladder", func(t *testing.T) {
		h := loadHistory(t, "gated-thresholds-gold-at-460.json")

		if !hasVersionMarker(h) || !hasNotifyActivity(h) {
			t.Fatalf("fixture is not a gated run at a promotion (marker=%v activity=%v)",
				hasVersionMarker(h), hasNotifyActivity(h))
		}
		if got := finalPoints(t, h); got != preMarkerBalance {
			t.Fatalf("fixture ends at %d points, not %d", got, preMarkerBalance)
		}

		if err := replay(t, h); err != nil {
			t.Errorf("a run recorded under the current ladder does not replay: %v", err)
		}
	})
}

// preMarkerBalance is the balance both threshold fixtures park on: gold under
// TiersV2, basic under TiersV1. Asserted to be inside that band, so a later edit
// to either threshold fails here rather than quietly leaving two fixtures that
// agree with each other.
const preMarkerBalance = 460

func TestTierFixturesStraddleTheThresholdChange(t *testing.T) {
	if rewards.TiersV1.Level(preMarkerBalance) != rewards.LevelBasic ||
		rewards.TiersV2.Level(preMarkerBalance) != rewards.LevelGold {
		t.Fatalf("%d points is %q under v1 and %q under v2; the threshold fixtures "+
			"no longer straddle the change and prove nothing about it", preMarkerBalance,
			rewards.TiersV1.Level(preMarkerBalance), rewards.TiersV2.Level(preMarkerBalance))
	}
}

// finalPoints reads the balance the last addPoints Update returned, which is how
// a fixture states what it is about without a comment anyone can forget to
// update.
func finalPoints(t *testing.T, h *historypb.History) int {
	t.Helper()
	points := 0
	for _, e := range h.GetEvents() {
		c := e.GetWorkflowExecutionUpdateCompletedEventAttributes()
		if c == nil {
			continue
		}
		var res rewards.AddPointsResult
		payloads := c.GetOutcome().GetSuccess().GetPayloads()
		if len(payloads) == 0 {
			continue
		}
		if err := converter.GetDefaultDataConverter().FromPayload(payloads[0], &res); err != nil {
			continue
		}
		if res.Balance > points {
			points = res.Balance
		}
	}
	if points == 0 {
		t.Fatal("no completed addPoints Update in the history; the fixture carries no balance")
	}
	return points
}

// The trap, pinned. worker.ReplayWorkflowHistory documents OriginalExecution as
// "optional", but for any workflow that reads its own ID it is not: the replayer
// substitutes "ReplayId", the enrollment check rejects the payload, and the run
// emits no commands at all.
//
// The failure is doubly misleading -- it reports nondeterminism, so it looks
// like a versioning problem, and it names whichever event came first, so it
// points at innocent code.
//
// Run against a history that replays perfectly well *with* the option, so the
// option is the only variable. Asserted rather than merely commented, so nobody
// "simplifies" replay() back to the plain call and gets a suite that passes
// while testing nothing.
func TestReplay_NeedsTheRecordedWorkflowID(t *testing.T) {
	h := loadHistory(t, "run-versioned-notification.json")
	if err := replay(t, h); err != nil {
		t.Fatalf("fixture does not replay even with OriginalExecution, so this "+
			"test cannot isolate the missing option: %v", err)
	}

	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflow(workflows.CustomerRewardsWorkflow)

	err := r.ReplayWorkflowHistory(nil, h)
	if err == nil {
		t.Fatal("the plain replay call now works -- drop the OriginalExecution workaround in replay()")
	}
	// TMPRL1100 is the SDK's nondeterminism class, and that is the whole
	// complaint: a harness mistake is reported as a versioning failure. Matched
	// on the code rather than the prose, which varies with whichever event the
	// replayer reached first.
	if !strings.Contains(err.Error(), "TMPRL1100") {
		t.Errorf("unexpected failure shape, worth re-reading: %v", err)
	}
}
