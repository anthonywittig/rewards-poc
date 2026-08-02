package activities

import (
	"context"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/activity"
)

type Activities struct {
	// A notification provider would be injected here in production.
}

func (a *Activities) NotifyCustomer(ctx context.Context, req rewards.NotifyRequest) error {
	activity.GetLogger(ctx).Info("notifying customer",
		"customerId", req.CustomerID,
		"event", req.Event,
		"level", req.Level,
		"idempotencyKey", req.IdempotencyKey,
	)

	// A notification provider would deliver here in production.
	return nil
}
