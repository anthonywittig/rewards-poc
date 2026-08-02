package rewards

// The tier-promotion notification contract. Only the Activity's *name* and
// *argument shape* live here, so the audit crawl can turn ActivityTaskScheduled
// / ActivityTaskCompleted into notification rows without guessing at either --
// changing the shape breaks the crawler's build rather than its output.

// ActivityNotifyCustomer is the registered name of the notification Activity.
// The audit crawl matches on it to distinguish notification rows from any other
// Activity a later phase might add, so it must equal what RegisterActivity uses.
const ActivityNotifyCustomer = "NotifyCustomer"

// Notification events. Promotion is the tier crossing; departure reuses the same
// Activity when a customer leaves.
const (
	NotifyEventPromoted = "promoted"
	NotifyEventDeparted = "departed"
)

// NotifyRequest is the NotifyCustomer argument.
type NotifyRequest struct {
	CustomerID string `json:"customerId"`
	Event      string `json:"event"`
	Level      string `json:"level"`
	// <customerID>:<level>. Stub ignores it; a real provider must honour it
	// because Activities are at-least-once.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}
