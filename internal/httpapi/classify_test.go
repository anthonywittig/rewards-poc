package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// The errors constructed below are the shapes a real server actually produced,
// captured by triggering each condition against the running stack rather than
// guessed from documentation. Several guesses were wrong on the first pass --
// worker-unavailable is FailedPrecondition rather than Unavailable, and a
// validator rejection arrives as an ApplicationError with an empty Type rather
// than as an untyped error -- which is why these are pinned here.

func status(t *testing.T, err error) (int, string) {
	t.Helper()
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return http.StatusInternalServerError, CodeInternal
	}
	return apiErr.status, apiErr.code
}

func TestMapStartError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantKind string
	}{
		{
			// Observed: "Workflow execution is already running. WorkflowId: ..."
			name:     "duplicate enrollment",
			err:      serviceerror.NewWorkflowExecutionAlreadyStarted("already running", "id", "run"),
			wantCode: http.StatusConflict,
			wantKind: CodeAlreadyExists,
		},
		{
			name:     "unknown failure falls through to 500",
			err:      errors.New("something nobody anticipated"),
			wantCode: http.StatusInternalServerError,
			wantKind: CodeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotKind := status(t, mapStartError(tc.err))
			if gotCode != tc.wantCode || gotKind != tc.wantKind {
				t.Errorf("got %d/%s, want %d/%s", gotCode, gotKind, tc.wantCode, tc.wantKind)
			}
		})
	}
}

func TestMapQueryError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantKind string
	}{
		{
			// Observed: "workflow not found for ID: customer-nobody-here"
			name:     "missing customer",
			err:      serviceerror.NewNotFound("workflow not found for ID: customer-x"),
			wantCode: http.StatusNotFound,
			wantKind: CodeNotFound,
		},
		{
			// Observed within ~10s of the worker stopping:
			// "no poller seen for task queue recently, worker may be down"
			name:     "no worker polling",
			err:      serviceerror.NewFailedPrecondition("no poller seen for task queue recently, worker may be down"),
			wantCode: http.StatusServiceUnavailable,
			wantKind: CodeWorkerUnavailable,
		},
		{
			// The same condition once the worker has been gone longer: the
			// query is simply never answered.
			name:     "query times out with no worker",
			err:      serviceerror.NewDeadlineExceeded("context deadline exceeded"),
			wantCode: http.StatusServiceUnavailable,
			wantKind: CodeWorkerUnavailable,
		},
		{
			name:     "temporal itself unreachable",
			err:      serviceerror.NewUnavailable("connection refused"),
			wantCode: http.StatusServiceUnavailable,
			wantKind: CodeWorkerUnavailable,
		},
		{
			// Anything unrecognised must become a logged 500 rather than being
			// guessed at -- this is what the query retry path keys off.
			name:     "unrecognised failure",
			err:      errors.New("Workflow task is not scheduled yet."),
			wantCode: http.StatusInternalServerError,
			wantKind: CodeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotKind := status(t, mapQueryError(tc.err))
			if gotCode != tc.wantCode || gotKind != tc.wantKind {
				t.Errorf("got %d/%s, want %d/%s", gotCode, gotKind, tc.wantCode, tc.wantKind)
			}
		})
	}
}

// Both halves of the validator/handler split (PLAN.md 3.4) must reach the caller
// as the same 422 carrying the workflow's own words. The caller is not supposed
// to be able to tell them apart -- that is the design.
func TestMapUpdateError_BothRejectionPathsAre422(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			// Observed: ApplicationError, Type() == "", NonRetryable false.
			name:    "validator rejection carries no type",
			err:     temporal.NewApplicationError("amount must be positive, got -5", ""),
			wantMsg: "amount must be positive, got -5",
		},
		{
			name:    "validator rejection: per-transaction maximum",
			err:     temporal.NewApplicationError("amount 5000 exceeds the per-transaction maximum of 1000", ""),
			wantMsg: "amount 5000 exceeds the per-transaction maximum of 1000",
		},
		{
			// Observed: ApplicationError, Type() == "PointsCapExceeded",
			// NonRetryable true. Message() excludes the SDK's "(type: ...)"
			// suffix, which is why it is what reaches the client.
			name: "handler rejection carries our type",
			err: temporal.NewNonRetryableApplicationError(
				"add of 500 would exceed the cap of 100000 (balance is 99999)",
				rewards.ErrTypePointsCapExceeded, nil),
			wantMsg: "add of 500 would exceed the cap of 100000 (balance is 99999)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapUpdateError(tc.err)
			gotCode, gotKind := status(t, mapped)
			if gotCode != http.StatusUnprocessableEntity || gotKind != CodeRejected {
				t.Fatalf("got %d/%s, want 422/%s", gotCode, gotKind, CodeRejected)
			}
			var apiErr *apiError
			errors.As(mapped, &apiErr)
			if apiErr.message != tc.wantMsg {
				t.Errorf("message = %q, want %q", apiErr.message, tc.wantMsg)
			}
		})
	}
}

// A rejection must never be reported as an outage: the workflow answered, and
// telling the caller "service unavailable" would invite a retry of a request
// that will be refused identically every time.
func TestMapUpdateError_RejectionBeatsOutageClassification(t *testing.T) {
	err := temporal.NewApplicationError("reason is required", "")
	gotCode, _ := status(t, mapUpdateError(err))
	if gotCode != http.StatusUnprocessableEntity {
		t.Errorf("got %d, want 422", gotCode)
	}
}

// An Update with no worker does not fail -- it blocks -- so it reaches the
// mapper as the timeout the API imposed, wrapped in the SDK's own type.
func TestMapUpdateError_TimeoutIsWorkerUnavailable(t *testing.T) {
	err := client.NewWorkflowUpdateServiceTimeoutOrCanceledError(context.DeadlineExceeded)
	gotCode, gotKind := status(t, mapUpdateError(err))
	if gotCode != http.StatusServiceUnavailable || gotKind != CodeWorkerUnavailable {
		t.Errorf("got %d/%s, want 503/%s", gotCode, gotKind, CodeWorkerUnavailable)
	}
}

func TestIsRolloverAbort(t *testing.T) {
	if isRolloverAbort(nil) {
		t.Error("nil is not an abort")
	}
	// A rejection is the workflow answering, not the run disappearing.
	if isRolloverAbort(temporal.NewApplicationError("reason is required", "")) {
		t.Error("a business rejection must not be treated as a rollover abort")
	}
	if !isRolloverAbort(errors.New("workflow execution already completed")) {
		t.Error("expected the completed-run wording to be recognised")
	}
}
