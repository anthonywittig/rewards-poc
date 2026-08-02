// Package activities holds every Activity in the system, which is to say every
// piece of this codebase permitted to touch the outside world.
//
// Activities are methods on a struct rather than bare functions so their
// dependencies can be injected once, at worker startup, instead of being reached
// for through package-level state. cmd/worker builds the struct.
package activities

import (
	"context"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/activity"
)

// Notifier is the seam a real notification provider drops into. The Activity
// owns the Temporal concerns and delegates only the delivery itself.
//
// The request carries the customer ID and no contact details: a real provider
// looks those up itself, which is what keeps them out of Event History, where
// payloads are readable in plaintext in the Temporal UI.
type Notifier interface {
	Notify(ctx context.Context, req rewards.NotifyRequest) error
}

// Activities is the Activity receiver, and the one place a dependency enters
// this system.
//
// Register it whole: RegisterActivity(&Activities{...}) registers every exported
// method under its own name -- so NotifyCustomer registers as
// rewards.ActivityNotifyCustomer, the string the workflow schedules by and the
// audit crawl matches on. Adding an exported method here adds an Activity.
type Activities struct {
	// Notifier delivers the message. Nil means log-only.
	Notifier Notifier
}

// NotifyCustomer is the only Activity in the system.
//
// IdempotencyKey is passed through and ignored by the stub. Activities are
// at-least-once -- a worker crash between this returning and its completion
// being recorded means it runs again on replay -- so a real Notifier would
// dedupe on that key.
func (a *Activities) NotifyCustomer(ctx context.Context, req rewards.NotifyRequest) error {
	activity.GetLogger(ctx).Info("notifying customer",
		"customerId", req.CustomerID,
		"event", req.Event,
		"level", req.Level,
		"idempotencyKey", req.IdempotencyKey,
	)

	if a.Notifier == nil {
		return nil
	}
	return a.Notifier.Notify(ctx, req)
}

// LogNotifier is the POC's delivery: the log line above and nothing else. It
// exists so cmd/worker can name what it injects rather than passing nil.
type LogNotifier struct{}

// Notify does nothing. NotifyCustomer has already logged the request.
func (LogNotifier) Notify(context.Context, rewards.NotifyRequest) error { return nil }
