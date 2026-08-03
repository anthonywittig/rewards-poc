package httpapi

import (
	"context"
	"errors"
	"strings"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// Error classification, derived by triggering each condition against a real
// server. Notable shapes: both halves of the validator/handler split arrive as
// *temporal.ApplicationError distinguished only by Type(), and an Update with
// no worker does not fail -- it blocks until our own updateTimeout fires.

// isWorkerUnavailable reports whether the failure means "nothing is polling
// the task queue".
func isWorkerUnavailable(err error) bool {
	var failedPre *serviceerror.FailedPrecondition
	if errors.As(err, &failedPre) {
		return true
	}

	// An Update with no worker blocks, so it reaches us as the deadline we
	// imposed, wrapped in the SDK's own type.
	var updTimeout *client.WorkflowUpdateServiceTimeoutOrCanceledError
	if errors.As(err, &updTimeout) {
		return true
	}
	return isTimeout(err)
}

// isTimeout covers both spellings of a deadline: the SDK's typed error for a
// server-side one, the stdlib sentinel for one our context imposed.
// context.Canceled is deliberately absent -- that is the caller hanging up.
func isTimeout(err error) bool {
	var deadline *serviceerror.DeadlineExceeded
	return errors.As(err, &deadline) || errors.Is(err, context.DeadlineExceeded)
}

// isBusinessRejection reports whether the workflow itself refused the request,
// as opposed to the request failing to reach it.
func isBusinessRejection(err error) (*temporal.ApplicationError, bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return nil, false
	}
	return appErr, true
}

// isClosedRun reports whether an Update failed because the run it targeted is
// no longer open. The same NotFound arises from continue-as-new (retry against
// the successor) and from a closed workflow (refuse); the caller disambiguates
// by asking what is running now.
func isClosedRun(err error) bool {
	if err == nil {
		return false
	}
	// A business rejection is the workflow answering, not the run disappearing.
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return false
	}
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// isHistoryGone reports whether a run's Event History has been deleted after
// retention. The audit crawl detects truncation by this error.
//
// Measured against a real server, a reaped run answers *InvalidArgument* with
// "...may have passed retention period." -- so the type alone cannot decide
// it, and this is the one place in the codebase that matches on message text.
// If a server upgrade changes the wording this surfaces as a loud 500 rather
// than a quietly shorter timeline, which is the right direction to fail in.
func isHistoryGone(err error) bool {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var invalid *serviceerror.InvalidArgument
	if !errors.As(err, &invalid) {
		return false
	}
	return strings.Contains(strings.ToLower(invalid.Error()), "retention period")
}
