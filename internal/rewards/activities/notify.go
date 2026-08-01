// Package activities holds every Activity in the system, which is to say every
// piece of this codebase permitted to touch the outside world. Points, tiers,
// enrollment and the audit log are all workflow state and need no side effects
// at all -- that is the argument the POC is making, and keeping the side effects
// in a package of their own is what makes the boundary visible.
//
// Activities are methods on a struct rather than bare functions so their
// dependencies can be injected once, at worker startup, instead of being reached
// for through package-level state. cmd/worker builds the struct; nothing else
// needs to know what is in it.
package activities

import (
	"context"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/activity"
)

// Notifier is the seam a real email or push provider drops into. The Activity
// owns the Temporal concerns -- logging, and what the workflow sees on failure --
// and delegates only the delivery itself, so swapping providers does not mean
// re-deciding any of that.
type Notifier interface {
	Notify(ctx context.Context, req rewards.NotifyRequest) error
}

// Activities is the Activity receiver, and the one place a dependency enters
// this system.
//
// Register it whole: w.RegisterActivity(&Activities{...}) registers every
// exported method under its own name, which is why NotifyCustomer keeps the name
// it had as a bare function -- rewards.ActivityNotifyCustomer, the string the
// workflow schedules by and the audit crawl matches on. Adding an exported
// method here adds an Activity; adding an unexported one does not.
type Activities struct {
	// Notifier delivers the message. Nil means log-only, which is what the POC
	// runs with -- see LogNotifier.
	Notifier Notifier
}

// NotifyCustomer is the only Activity in the system. PLAN.md 3.7.
//
// The default delivery is a stub -- production would inject a Notifier that
// calls an email or push provider -- but everything around it is real: it is
// scheduled by workflow code, retried by the platform, and recorded in Event
// History, which is what makes the audit timeline pick up "notification sent"
// rows for free (PLAN.md 6.2).
//
// IdempotencyKey is passed through and, by the stub, ignored -- which is the
// honest shape for a stub: Activities are at-least-once, so a worker crash
// between this returning and its completion being recorded means it runs again
// on replay. A real Notifier would dedupe on that key. Documenting the contract
// in the type is worth more than pretending the stub honours it.
func (a *Activities) NotifyCustomer(ctx context.Context, req rewards.NotifyRequest) error {
	activity.GetLogger(ctx).Info("notifying customer",
		"customerId", req.CustomerID,
		"email", req.Email,
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
// exists so cmd/worker can name what it is injecting rather than passing nil and
// leaving the reader to work out that nil means "no provider wired up yet".
type LogNotifier struct{}

// Notify does nothing. NotifyCustomer has already logged the request.
func (LogNotifier) Notify(context.Context, rewards.NotifyRequest) error { return nil }
