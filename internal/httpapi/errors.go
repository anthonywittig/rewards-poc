package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Stable machine-readable error codes. Clients should switch on these rather
// than on message text.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeAlreadyExists     = "already_exists"
	CodeNotFound          = "not_found"
	CodeRejected          = "rejected"
	CodeWorkerUnavailable = "worker_unavailable"
	CodeRolloverRace      = "rollover_race"
	CodeDeactivated       = "deactivated"
	CodeInternal          = "internal"
)

// apiError is a mapped, client-facing failure.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(msg string) *apiError {
	return &apiError{http.StatusBadRequest, CodeInvalidRequest, msg}
}

// writeError renders an apiError, mapping anything unrecognised to a 500 while
// logging the original. An unmapped error reaching a client as a raw gRPC string
// is the failure mode this whole file exists to prevent.
func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		log.Error("unmapped error", "error", err)
		apiErr = &apiError{http.StatusInternalServerError, CodeInternal, "internal error"}
	}
	writeJSON(w, log, apiErr.status, ErrorResponse{ErrorBody{apiErr.code, apiErr.message}})
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers are already sent, so this cannot become a response.
		log.Error("failed writing response body", "error", err)
	}
}

// mapStartError turns a failed ExecuteWorkflow into an HTTP status.
//
// The 409 depends on the client being configured with
// WorkflowExecutionErrorWhenAlreadyStarted -- without it the SDK returns the
// existing run and a nil error, and there is nothing here to map. PLAN.md 3.6.
//
// AlreadyStarted also covers a *departed* customer, whose ID the reuse policy
// refuses to hand out again -- and "already enrolled and active" is plainly
// wrong for them. Telling the two apart needs a Describe, which is why the
// enroll path goes through Server.mapEnrollError and only lands here once the
// answer is known (or is unavailable).
func mapStartError(err error) error {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return &apiError{http.StatusConflict, CodeAlreadyExists,
			"customer is already enrolled and active"}
	}
	return classifyCommon(err)
}

// mapQueryError turns a failed QueryWorkflow or Describe into an HTTP status.
func mapQueryError(err error) error {
	return classifyCommon(err)
}

// mapUpdateError turns a failed UpdateWorkflow into an HTTP status.
//
// The interesting case is 422. Both halves of the validator/handler split
// (PLAN.md 3.4) surface here as a failed Update, and the caller is meant to be
// unable to tell them apart -- which is exactly right for the API too. What
// matters is separating a *business* rejection from an *infrastructure* failure,
// because only the first is the caller's fault.
func mapUpdateError(err error) error {
	// Checked before the common classifier: a rejection is the workflow
	// answering, and must never be reported as an outage.
	if appErr, ok := isBusinessRejection(err); ok {
		// appErr.Message() is the workflow's own words without the SDK's
		// "(type: ..., retryable: ...)" suffix, which is exactly what belongs
		// in a 422 body.
		return &apiError{http.StatusUnprocessableEntity, CodeRejected, appErr.Message()}
	}
	return classifyCommon(err)
}

// mapStoreReadError classifies failures for the two endpoints that read *stored*
// data rather than asking a running workflow: the customer list, which reads the
// visibility index, and the audit crawl, which reads Event History. Neither
// involves a worker at any point.
//
// subject names the operation that ran out of time, e.g. "the customer list".
//
// Without this, both inherit the Query path's wording -- "the rewards workflow
// did not respond in time; the worker may be down or overloaded" -- which names
// two things neither endpoint touches. Sending someone to restart a worker
// because the visibility store was slow is exactly the mistake the
// FailedPrecondition wording made before PR #6 split status from message; this
// is the same fix applied to two call sites that were missed at the time.
//
// The status stays 503. Slow is transient and retrying is right, so nothing the
// caller *does* changes -- only what they are told to go and look at.
//
// The code stays CodeWorkerUnavailable, which reads oddly here. It is deliberate:
// the error contract is frozen so Phase 8 can build against it (PLAN.md 5.1), and
// this is the only 503 in it. Clients treat it as "backend not ready, retry",
// which is correct for a slow visibility store as much as for a missing worker.
// A truer code would be worth having and is not worth breaking the freeze for.
func mapStoreReadError(err error, subject string) error {
	if isTimeout(err) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			subject + " did not finish in time; temporal is slow or unreachable"}
	}
	return classifyCommon(err)
}

// classifyCommon handles the failures every endpoint shares.
func classifyCommon(err error) error {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return &apiError{http.StatusNotFound, CodeNotFound,
			"customer not found, or their history has been deleted"}
	}

	// No worker polling is the single most common development-time failure.
	// PLAN.md 12.4.
	if isWorkerUnavailable(err) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			workerUnavailableMessage(err)}
	}

	var unavailable *serviceerror.Unavailable
	if errors.As(err, &unavailable) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"temporal is unavailable"}
	}

	return err // becomes a logged 500
}

// workerUnavailableMessage picks how confidently to word a 503.
//
// The status is the same either way -- all of these are transient server-side
// conditions worth retrying -- but the *message* should only name the worker
// when the server actually said so. FailedPrecondition covers more than a
// missing poller, and telling someone to check `make worker` while their worker
// is fine sends them down the wrong path.
func workerUnavailableMessage(err error) string {
	if mentionsNoPoller(err.Error()) {
		return "no worker is polling the rewards task queue; is `make worker` running?"
	}
	var deadline *serviceerror.DeadlineExceeded
	var updTimeout *client.WorkflowUpdateServiceTimeoutOrCanceledError
	if errors.As(err, &deadline) || errors.As(err, &updTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "the rewards workflow did not respond in time; the worker may be down or overloaded"
	}
	return "temporal cannot serve this request right now: " + err.Error()
}
