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
// The two useful surprises are that both halves of the validator/handler split
// arrive as ApplicationError distinguished only by Type() -- so no message
// matching is needed to tell a business rejection from a transport failure --
// and that a no-worker Update does not fail at all where a Query fails fast.

// isWorkerUnavailable reports whether the failure is "nothing is polling the
// task queue", which during development is by far the most common cause and
// whose native message ("no poller seen for task queue recently, worker may be
// down") deserves to reach the client intact rather than as a 500. PLAN.md 12.4.
func isWorkerUnavailable(err error) bool {
	// FailedPrecondition is Temporal's "cannot do this right now", of which
	// "no poller seen for task queue recently, worker may be down" is only one
	// instance. Treating the whole type as 503-is-retryable is right -- these are
	// transient server-side conditions, not caller mistakes -- but claiming the
	// worker specifically is not, which is why the message is chosen separately
	// in workerUnavailableMessage rather than assumed here.
	var failedPre *serviceerror.FailedPrecondition
	if errors.As(err, &failedPre) {
		return true
	}

	// An Update with no worker does not fail -- it blocks -- so it reaches us as
	// the deadline we imposed in updateTimeout, wrapped in the SDK's own type.
	// That type also covers client-side cancellation, which would be a caller
	// disappearing rather than a worker being absent; the distinction does not
	// matter here, because a caller who hung up is not reading the response.
	var updTimeout *client.WorkflowUpdateServiceTimeoutOrCanceledError
	if errors.As(err, &updTimeout) {
		return true
	}
	return isTimeout(err)
}

// isTimeout reports whether a call ran out of the time we gave it.
//
// Both spellings matter: the SDK surfaces a server-side deadline as its own
// typed error, while a deadline our own context imposed arrives as the stdlib
// sentinel. Note context.Canceled is deliberately absent -- that is the caller
// hanging up, and nobody is left to read the response.
func isTimeout(err error) bool {
	var deadline *serviceerror.DeadlineExceeded
	return errors.As(err, &deadline) || errors.Is(err, context.DeadlineExceeded)
}

// isBusinessRejection reports whether the workflow itself refused the request,
// as opposed to the request failing to reach it.
//
// Both halves of PLAN.md 3.4 land here: a validator rejection arrives as an
// ApplicationError with an empty Type, a handler rejection with the type we
// chose. They are deliberately indistinguishable to the caller -- which is the
// design, not a limitation -- so both become 422. What matters is that neither
// is confused with an outage.
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
// This is deliberately NOT called "isRolloverAbort", which is what an earlier
// version got wrong. The error is ambiguous, and the same NotFound arises from
// two situations that need opposite responses:
//
//	continue-as-new  -- the old run closed, a successor is running   -> retry
//	deactivation     -- the customer cancelled, nothing is running   -> refuse
//
// Nothing in the error distinguishes them, because in both cases the run the
// update addressed really has completed. Matching on the message alone sent
// "please retry" to callers adding points to a departed customer, where no
// number of retries can ever succeed. Resolving it requires asking the server
// what is running now -- see updateWithRolloverRetry.
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
// PLAN.md 6.3 predicted this arrives as NotFound. It does not, and the
// difference is not cosmetic: the audit crawl detects truncation by *this
// error*, so with the predicted classification a truncated log came back as an
// unmapped 500 instead of the timeline it was designed to serve. Measured
// against the real server, GetWorkflowHistory answers:
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
// that matches on message text -- everywhere else that would be a bug. There is
// no other signal: "history deleted" and "run ID you made up" are the same error
// because they are, from the server's side, the same situation.
//
// Note the server says "may have passed retention period" even when the run was
// explicitly deleted, which for `make reap` is a guess and a wrong one. The
// substring below is chosen from the half of the sentence that is diagnostic
// rather than speculative.
//
// If a server upgrade changes that wording, truncation stops being recognised
// and starts surfacing as a 500. That is the direction to fail in -- a loud
// error beats a timeline that quietly shows fewer rows than the customer has,
// which is precisely the outcome PLAN.md 6.3 exists to prevent.
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
