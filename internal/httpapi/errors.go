package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.temporal.io/api/serviceerror"
)

// Stable machine-readable error codes. Clients switch on these, not on
// message text.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeAlreadyExists     = "already_exists"
	CodeNotFound          = "not_found"
	CodeRejected          = "rejected"
	CodeWorkerUnavailable = "worker_unavailable"
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

// writeError renders an apiError, mapping anything unrecognised to a logged
// 500 so no raw gRPC string reaches a client.
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

// mapUpdateError turns a failed UpdateWorkflow into an HTTP status. Business
// rejections become 422; everything else shares the common classification.
func mapUpdateError(err error) error {
	if appErr, ok := isBusinessRejection(err); ok {
		return &apiError{http.StatusUnprocessableEntity, CodeRejected, appErr.Message()}
	}
	return classifyCommon(err)
}

// mapStoreReadError classifies failures for reads of stored data (the list,
// the audit crawl, Describe). No worker is involved there, so a timeout must
// not blame one.
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

	// No worker polling is the most common development-time failure, so the
	// 503 leads with the fix.
	if isWorkerUnavailable(err) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"no worker is polling the rewards task queue; is the stack up? try `make up`"}
	}

	var unavailable *serviceerror.Unavailable
	if errors.As(err, &unavailable) {
		return &apiError{http.StatusServiceUnavailable, CodeWorkerUnavailable,
			"temporal is unavailable"}
	}

	return err // becomes a logged 500
}
