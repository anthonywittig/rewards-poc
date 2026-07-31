package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.temporal.io/api/serviceerror"
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
			"no worker is polling the rewards task queue, or it is not responding; is `make worker` running?"}
	}

	var unavailable *serviceerror.Unavailable
	if errors.As(err, &unavailable) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"temporal is unavailable"}
	}

	return err // becomes a logged 500
}
