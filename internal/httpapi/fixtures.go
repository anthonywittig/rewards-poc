package httpapi

import "time"

// Fixture data for cmd/mockapi, so the UI (Phase 8) can be built against the
// frozen contract before Phases 4 and 5 exist.
//
// Chosen to exercise the cases that are easy to forget and awkward to reproduce
// on demand against a real stack: a deactivated customer, a truncated audit log,
// a handler rejection, a tier promotion, and generation boundaries. Building a
// UI only against a freshly-enrolled happy-path customer is how "showing 7 of
// 23" ends up untested until someone reaps a run by hand.
//
// Deliberately in the same package as the DTOs so the mock cannot drift from the
// contract without failing to compile.

var fixtureNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return fixtureNow.Add(-d) }

// FixtureCustomers is the mock's whole world, keyed by customer ID.
var FixtureCustomers = map[string]CustomerResponse{
	// Ordinary active customer, mid-tier, a few rollovers in.
	"ada": {
		CustomerID: "ada", Name: "Ada Lovelace", Email: "ada@example.com",
		Points: 640, Level: "gold", NextTierAt: 1000,
		EnrolledAt:         ago(30 * 24 * time.Hour),
		LifetimeEarnEvents: 8, Generation: 2,
		Status: "active", RunID: "019fb9c1-0000-0000-0000-000000000003",
	},
	// Top tier: NextTierAt is 0, which the progress bar must handle.
	"grace": {
		CustomerID: "grace", Name: "Grace Hopper", Email: "grace@example.com",
		Points: 1500, Level: "platinum", NextTierAt: 0,
		EnrolledAt:         ago(90 * 24 * time.Hour),
		LifetimeEarnEvents: 21, Generation: 7,
		Status: "active", RunID: "019fb9c1-0000-0000-0000-000000000007",
	},
	// Brand new: zero everything, generation 0. The empty-timeline case.
	"newbie": {
		CustomerID: "newbie", Name: "Newly Enrolled", Email: "new@example.com",
		Points: 0, Level: "basic", NextTierAt: 500,
		EnrolledAt:         ago(2 * time.Minute),
		LifetimeEarnEvents: 0, Generation: 0,
		Status: "active", RunID: "019fb9c1-0000-0000-0000-000000000010",
	},
	// Sitting just under the 100,000 cap, so any add over 40 gets a *handler*
	// rejection -- which, unlike a validator rejection, appears in the audit
	// log. Without a fixture like this the UI's rejected-row rendering never
	// gets exercised, because reaching the cap organically takes 100 adds.
	"capped": {
		CustomerID: "capped", Name: "Max Capacity", Email: "max@example.com",
		Points: 99960, Level: "platinum", NextTierAt: 0,
		EnrolledAt:         ago(200 * 24 * time.Hour),
		LifetimeEarnEvents: 412, Generation: 137,
		Status: "active", RunID: "019fb9c1-0000-0000-0000-000000000137",
	},
	// Departed. Points and tier are their finals; the UI must not offer an
	// add-points form, and re-enrolling starts from zero (PLAN.md 3.6).
	"departed": {
		CustomerID: "departed", Name: "Gone Away", Email: "gone@example.com",
		Points: 310, Level: "basic", NextTierAt: 500,
		EnrolledAt:         ago(60 * 24 * time.Hour),
		LifetimeEarnEvents: 4, Generation: 1,
		Status: "deactivated", RunID: "019fb9c1-0000-0000-0000-000000000004",
	},
}

// FixtureAudits mirrors FixtureCustomers. "grace" is deliberately truncated.
var FixtureAudits = map[string]AuditResponse{
	"ada": {
		CustomerID: "ada", Truncated: false,
		ShownEarnEvents: 8, LifetimeEarnEvents: 8, RunsWalked: 3,
		Entries: []AuditEntry{
			{Kind: AuditEnrolled, At: ago(30 * 24 * time.Hour), Generation: 0, RunID: "…0001", EventID: 1},
			{Kind: AuditPointsAdded, At: ago(29 * 24 * time.Hour), Generation: 0, RunID: "…0001", EventID: 9,
				Amount: 120, Reason: "signup bonus", Balance: 120, Level: "basic", RequestID: "req-1"},
			{Kind: AuditPointsAdded, At: ago(25 * 24 * time.Hour), Generation: 0, RunID: "…0001", EventID: 15,
				Amount: 200, Reason: "purchase", Balance: 320, Level: "basic", RequestID: "req-2"},
			{Kind: AuditPointsAdded, At: ago(21 * 24 * time.Hour), Generation: 0, RunID: "…0001", EventID: 21,
				Amount: 180, Reason: "purchase", Balance: 500, Level: "gold", RequestID: "req-3"},
			// Promotion notification lands in the audit log for free, because
			// Activities are history events. PLAN.md 3.7.
			{Kind: AuditNotificationSent, At: ago(21 * 24 * time.Hour), Generation: 0, RunID: "…0001", EventID: 24,
				NotifiedLevel: "gold"},
			{Kind: AuditGenerationRolled, At: ago(21 * 24 * time.Hour), Generation: 1, RunID: "…0002", EventID: 28},
			{Kind: AuditPointsAdded, At: ago(14 * 24 * time.Hour), Generation: 1, RunID: "…0002", EventID: 9,
				Amount: 60, Reason: "referral", Balance: 560, Level: "gold", RequestID: "req-4"},
			// A handler-side rejection: recorded, unlike a validator rejection,
			// which writes nothing at all. PLAN.md 3.4.
			{Kind: AuditPointsRejected, At: ago(13 * 24 * time.Hour), Generation: 1, RunID: "…0002", EventID: 15,
				Amount: 999999, Reason: "bulk credit", RequestID: "req-5",
				Failure: "add of 999999 would exceed the cap of 100000 (balance is 560)"},
			{Kind: AuditPointsAdded, At: ago(10 * 24 * time.Hour), Generation: 1, RunID: "…0002", EventID: 21,
				Amount: 40, Reason: "purchase", Balance: 600, Level: "gold", RequestID: "req-6"},
			{Kind: AuditGenerationRolled, At: ago(10 * 24 * time.Hour), Generation: 2, RunID: "…0003", EventID: 25},
			{Kind: AuditPointsAdded, At: ago(5 * 24 * time.Hour), Generation: 2, RunID: "…0003", EventID: 9,
				Amount: 20, Reason: "purchase", Balance: 620, Level: "gold", RequestID: "req-7"},
			{Kind: AuditPointsAdded, At: ago(2 * 24 * time.Hour), Generation: 2, RunID: "…0003", EventID: 15,
				Amount: 20, Reason: "purchase", Balance: 640, Level: "gold", RequestID: "req-8"},
		},
	},
	// The truncation case, and the reason it exists: 21 lifetime earns but only
	// 3 reconstructable, because everything older was reaped. The UI must render
	// "Showing 3 of 21 point events. Earlier history has been deleted."
	"grace": {
		CustomerID: "grace", Truncated: true,
		ShownEarnEvents: 3, LifetimeEarnEvents: 21, RunsWalked: 2,
		OldestRunID: "019fb9c1-0000-0000-0000-000000000006",
		Entries: []AuditEntry{
			{Kind: AuditPointsAdded, At: ago(3 * 24 * time.Hour), Generation: 6, RunID: "…0006", EventID: 15,
				Amount: 100, Reason: "purchase", Balance: 1400, Level: "platinum", RequestID: "req-19"},
			{Kind: AuditGenerationRolled, At: ago(2 * 24 * time.Hour), Generation: 7, RunID: "…0007", EventID: 19},
			{Kind: AuditPointsAdded, At: ago(1 * 24 * time.Hour), Generation: 7, RunID: "…0007", EventID: 9,
				Amount: 50, Reason: "purchase", Balance: 1450, Level: "platinum", RequestID: "req-20"},
			{Kind: AuditPointsAdded, At: ago(6 * time.Hour), Generation: 7, RunID: "…0007", EventID: 15,
				Amount: 50, Reason: "purchase", Balance: 1500, Level: "platinum", RequestID: "req-21"},
		},
	},
	// Long-lived and heavily truncated: 412 lifetime earns, 2 reconstructable.
	// The extreme end of the "showing N of M" message.
	"capped": {
		CustomerID: "capped", Truncated: true,
		ShownEarnEvents: 2, LifetimeEarnEvents: 412, RunsWalked: 1,
		OldestRunID: "019fb9c1-0000-0000-0000-000000000137",
		Entries: []AuditEntry{
			{Kind: AuditPointsAdded, At: ago(4 * time.Hour), Generation: 137, RunID: "…0137", EventID: 9,
				Amount: 20, Reason: "purchase", Balance: 99940, Level: "platinum", RequestID: "req-411"},
			{Kind: AuditPointsAdded, At: ago(2 * time.Hour), Generation: 137, RunID: "…0137", EventID: 15,
				Amount: 20, Reason: "purchase", Balance: 99960, Level: "platinum", RequestID: "req-412"},
		},
	},
	// Enrolled, never earned. Timeline with a single row.
	"newbie": {
		CustomerID: "newbie", Truncated: false,
		ShownEarnEvents: 0, LifetimeEarnEvents: 0, RunsWalked: 1,
		Entries: []AuditEntry{
			{Kind: AuditEnrolled, At: ago(2 * time.Minute), Generation: 0, RunID: "…0010", EventID: 1},
		},
	},
	// Ends with a deactivation row.
	"departed": {
		CustomerID: "departed", Truncated: false,
		ShownEarnEvents: 4, LifetimeEarnEvents: 4, RunsWalked: 2,
		Entries: []AuditEntry{
			{Kind: AuditEnrolled, At: ago(60 * 24 * time.Hour), Generation: 0, RunID: "…0003", EventID: 1},
			{Kind: AuditPointsAdded, At: ago(58 * 24 * time.Hour), Generation: 0, RunID: "…0003", EventID: 9,
				Amount: 100, Reason: "signup bonus", Balance: 100, Level: "basic", RequestID: "req-a"},
			{Kind: AuditPointsAdded, At: ago(50 * 24 * time.Hour), Generation: 0, RunID: "…0003", EventID: 15,
				Amount: 100, Reason: "purchase", Balance: 200, Level: "basic", RequestID: "req-b"},
			{Kind: AuditPointsAdded, At: ago(45 * 24 * time.Hour), Generation: 0, RunID: "…0003", EventID: 21,
				Amount: 100, Reason: "purchase", Balance: 300, Level: "basic", RequestID: "req-c"},
			{Kind: AuditGenerationRolled, At: ago(45 * 24 * time.Hour), Generation: 1, RunID: "…0004", EventID: 25},
			{Kind: AuditPointsAdded, At: ago(40 * 24 * time.Hour), Generation: 1, RunID: "…0004", EventID: 9,
				Amount: 10, Reason: "purchase", Balance: 310, Level: "basic", RequestID: "req-d"},
			{Kind: AuditDeactivated, At: ago(35 * 24 * time.Hour), Generation: 1, RunID: "…0004", EventID: 14},
		},
	},
}

// FixtureListItem projects a customer down to what ListWorkflow could actually
// return -- search attributes only. Keeping this a projection rather than a
// separate literal is what stops the mock from advertising fields the real
// Phase 4 endpoint will not have.
func FixtureListItem(c CustomerResponse) CustomerListItem {
	return CustomerListItem{
		CustomerID: c.CustomerID,
		Name:       c.Name,
		Email:      c.Email,
		Points:     c.Points,
		Level:      c.Level,
		EnrolledAt: c.EnrolledAt,
		Generation: c.Generation,
		Status:     c.Status,
		RunID:      c.RunID,
	}
}
