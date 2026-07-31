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
	// "no poller seen for task queue recently, worker may be down".
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

// isRolloverAbort reports whether an Update was aborted because the run it
// targeted continued-as-new underneath it.
//
// NOT captured empirically, unlike everything else in this file. Reproducing it
// needs an Update in flight at the instant a run rolls, and attempts to force
// that window open did not land it. The matching below is therefore a
// best-effort guess at the wording and may simply never fire -- in which case
// the request surfaces as a 500 rather than being retried. Recorded as an
// exposure in PLAN.md 12.13 rather than presented as verified.
func isRolloverAbort(err error) bool {
	if err == nil {
		return false
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		// A rejection carries a workflow-authored message; it is not an abort.
		return false
	}
	return containsAny(err.Error(),
		"workflow execution already completed",
		"update was aborted",
		"UpdateAborted",
	)
}

func containsAny(s string, subs ...string) bool {
	l := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(l, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
