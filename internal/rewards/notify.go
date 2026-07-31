package rewards

// The tier-promotion notification contract. The Activity itself is Phase 6
// (PLAN.md 3.7); only its *name* and *argument shape* live here, because the
// Phase 5 audit crawl needs both to turn `ActivityTaskScheduled` /
// `ActivityTaskCompleted` into notification rows.
//
// Declaring them a phase early is the same trade already made for
// CustomerState.NotifiedLevels: the alternative is for the crawler to guess at
// an activity name and payload shape it cannot compile against, which is how
// "the audit log picks it up for free" (PLAN.md 3.7) quietly becomes false.
// Sharing the type means Phase 6 cannot change the shape without the crawler
// failing to build.

// ActivityNotifyCustomer is the registered name of the notification Activity.
// The audit crawl matches on it to distinguish notification rows from any other
// Activity a later phase might add, so it must equal what RegisterActivity uses.
const ActivityNotifyCustomer = "NotifyCustomer"

// Notification events. Promotion is the tier crossing; departure reuses the
// same Activity when a customer leaves (PLAN.md 3.7).
const (
	NotifyEventPromoted = "promoted"
	NotifyEventDeparted = "departed"
)

// NotifyRequest is the NotifyCustomer argument.
//
// IdempotencyKey is <customerID>:<level> -- a customer reaches gold exactly
// once, so that is a natural key. The stub will ignore it; it is here because a
// real notification service would honour it, and Activities are at-least-once.
type NotifyRequest struct {
	CustomerID     string `json:"customerId"`
	Email          string `json:"email"`
	Event          string `json:"event"`
	Level          string `json:"level"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}
