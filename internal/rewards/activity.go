package rewards

import (
	"context"

	"go.temporal.io/sdk/activity"
)

// NotifyCustomer is the only Activity in the system. PLAN.md 3.7.
//
// The body is a stub -- production would call an email or push provider here --
// but everything around it is real: it is scheduled by workflow code, retried by
// the platform, and recorded in Event History, which is what makes the audit
// timeline pick up "notification sent" rows for free (PLAN.md 6.2).
//
// It is deliberately the *only* thing in this codebase that would touch the
// outside world. Everything else -- points, tiers, enrollment, the audit log --
// is workflow state and needs no side effects at all, which is the argument the
// POC is making. Having exactly one Activity makes the boundary visible.
//
// IdempotencyKey is passed and then ignored, which is the honest shape for a
// stub: Activities are at-least-once, so a worker crash between this returning
// and its completion being recorded means it runs again on replay. A real
// notifier would dedupe on that key. Documenting the contract in the signature
// is worth more than pretending the stub honours it.
func NotifyCustomer(ctx context.Context, req NotifyRequest) error {
	activity.GetLogger(ctx).Info("notifying customer",
		"customerId", req.CustomerID,
		"email", req.Email,
		"event", req.Event,
		"level", req.Level,
		"idempotencyKey", req.IdempotencyKey,
	)
	return nil
}
