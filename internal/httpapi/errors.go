package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

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
// logging the original, so no raw gRPC string can reach a client.
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
// existing run and a nil error, and there is nothing here to map.
// FINDINGS.md#soft-deactivation.
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
// Both halves of the validator/handler split
// (FINDINGS.md#the-validatorhandler-split) surface here as a 422; what matters
// is separating a business rejection from an infrastructure failure.
//
// Deactivated is the exception: a business answer, but a 409 with its own code
// so clients can offer re-enrollment rather than treating it like a bad amount.
func mapUpdateError(err error) error {
	if appErr, ok := isBusinessRejection(err); ok {
		if appErr.Type() == rewards.ErrTypeDeactivated {
			return &apiError{http.StatusConflict, CodeDeactivated, appErr.Message()}
		}
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
// Without it both inherit the Query path's wording, which blames a worker
// neither endpoint touches.
//
// The code stays CodeWorkerUnavailable, which reads oddly here, because the
// error contract is frozen (FINDINGS.md#no-pagination-and-a-frozen-contract) and
// this is the only 503 in it. Clients treat it as "backend not ready, retry".
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
	// FINDINGS.md#read-and-write-timeouts.
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

// workerUnavailableMessage picks how confidently to word a 503. The status is
// the same either way; the message names the worker only when the server did.
// FailedPrecondition covers more than a missing poller.
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
