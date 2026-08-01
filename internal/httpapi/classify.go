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
// server rather than by reading documentation. The observed shapes:
//
//	condition                     Go type                                    Type()
//	---------------------------   ----------------------------------------   ------------------
//	missing customer              *serviceerror.NotFound                     --
//	duplicate enrollment          *serviceerror.WorkflowExecutionAlreadyStarted  --
//	validator rejection           *temporal.ApplicationError                 "" (empty)
//	handler rejection             *temporal.ApplicationError                 "PointsCapExceeded"
//	no worker polling (query)     *serviceerror.FailedPrecondition           --
//	no worker polling (update)    <blocks forever -- see updateTimeout>
//
// The two useful surprises: both halves of the validator/handler split arrive as
// ApplicationError distinguished only by Type(), and a no-worker Update does not
// fail at all where a Query fails fast.

// isWorkerUnavailable reports whether the failure is "nothing is polling the task
// queue". FINDINGS.md#read-and-write-timeouts.
func isWorkerUnavailable(err error) bool {
	// FailedPrecondition covers more than a missing poller. The whole type is
	// 503-is-retryable, but only workerUnavailableMessage decides whether to
	// blame the worker by name.
	var failedPre *serviceerror.FailedPrecondition
	if errors.As(err, &failedPre) {
		return true
	}

	// An Update with no worker does not fail -- it blocks -- so it reaches us as
	// the deadline we imposed in updateTimeout, wrapped in the SDK's own type.
	var updTimeout *client.WorkflowUpdateServiceTimeoutOrCanceledError
	if errors.As(err, &updTimeout) {
		return true
	}
	return isTimeout(err)
}

// isTimeout reports whether a call ran out of the time we gave it. Both
// spellings matter: the SDK surfaces a server-side deadline as its own typed
// error, a deadline our context imposed as the stdlib sentinel. context.Canceled
// is deliberately absent -- that is the caller hanging up.
func isTimeout(err error) bool {
	var deadline *serviceerror.DeadlineExceeded
	return errors.As(err, &deadline) || errors.Is(err, context.DeadlineExceeded)
}

// isBusinessRejection reports whether the workflow itself refused the request,
// as opposed to the request failing to reach it. Both halves of
// FINDINGS.md#the-validatorhandler-split land here and both become 422; what
// matters is that neither is confused with an outage.
func isBusinessRejection(err error) (*temporal.ApplicationError, bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return nil, false
	}
	return appErr, true
}

// isClosedRun reports whether an Update failed because the run it targeted is no
// longer open. Observed as *serviceerror.NotFound carrying "workflow execution
// already completed".
//
// Deliberately NOT called "isRolloverAbort": the same NotFound arises from two
// situations needing opposite responses, and nothing in the error tells them
// apart.
//
//	continue-as-new  -- the old run closed, a successor is running   -> retry
//	deactivation     -- the customer left, nothing is running        -> refuse
//
// Resolving it requires asking the server what is running now -- see
// updateWithRolloverRetry.
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

// isHistoryGone reports whether a run's Event History has been deleted --
// reaped after retention, or removed on demand by `make reap`.
//
// The audit crawl detects truncation by *this error*, so getting it wrong turns
// a truncated log into an unmapped 500. Measured against the real server,
// GetWorkflowHistory answers:
//
//	condition                        Go type                          message
//	------------------------------   ------------------------------   -----------------------------------
//	run reaped                       *serviceerror.InvalidArgument    "Requested workflow history not
//	                                                                   found, may have passed retention
//	                                                                   period."
//	run ID well-formed, never used   *serviceerror.InvalidArgument    (identical to the above)
//	run ID malformed                 *serviceerror.InvalidArgument    "Invalid RunId."
//	workflow ID never existed        *serviceerror.NotFound           "workflow not found for ID: ..."
//
// So the type alone cannot decide it, and this is the one place in the codebase
// that matches on message text -- everywhere else that would be a bug.
//
// If a server upgrade changes that wording, truncation stops being recognised
// and surfaces as a 500. That is the direction to fail in: a loud error beats a
// timeline that quietly shows fewer rows than the customer has.
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

// mentionsNoPoller reports whether a message names the specific condition the
// worker-unavailable 503 claims. Used to decide how *confidently* to word that
// response, never to decide the status code.
func mentionsNoPoller(msg string) bool {
	l := strings.ToLower(msg)
	for _, sub := range []string{"no poller", "no workers", "worker may be down"} {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
