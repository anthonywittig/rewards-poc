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

	// The same condition, reported differently depending on how long the worker
	// has been gone -- a query that nobody answers eventually runs out of time
	// rather than being refused. See queryTimeout for the measurements.
	var deadline *serviceerror.DeadlineExceeded
	if errors.As(err, &deadline) {
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
	return errors.Is(err, context.DeadlineExceeded)
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
