package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

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

// mapQueryError turns a failed QueryWorkflow or Describe into an HTTP status.
func mapQueryError(err error) error {
	return classifyCommon(err)
}

// mapUpdateError turns a failed UpdateWorkflow into an HTTP status.
//
// Both halves of the validator/handler split surface here as a 422; what
// matters is separating a business rejection from an infrastructure failure.
//
// Deactivated is the exception: a business answer, but a 409 with its own code
// so clients can say the membership has ended for good rather than treating it
// like a bad amount.
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
// involves a worker at any point, so a timeout here must not send anyone off to
// restart one.
//
// The code stays CodeWorkerUnavailable, which reads oddly here, because the
// error contract is frozen and this is the only 503 in it. Clients treat it as "backend not ready, retry".
func mapStoreReadError(err error) error {
	if isTimeout(err) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"the read did not finish in time; temporal is slow or unreachable"}
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

	// No worker polling is the single most common development-time failure, so
	// the 503 leads with the fix. FailedPrecondition and a timed-out call cover
	// more conditions than a missing poller, but pointing at the worker first is
	// right far more often than it is wrong in development.
	if isWorkerUnavailable(err) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"no worker is polling the rewards task queue; is `make worker` running?"}
	}

	var unavailable *serviceerror.Unavailable
	if errors.As(err, &unavailable) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"temporal is unavailable"}
	}

	return err // becomes a logged 500
}
