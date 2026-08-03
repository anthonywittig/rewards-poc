package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// The errors constructed below are the shapes a real server produced, captured
// by triggering each condition against the running stack. Documentation and
// guesswork got several of them wrong, which is why they are pinned here.

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

// Both halves of the validator/handler split must reach the caller as the same
// 422 carrying the workflow's own words.
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

// Deactivated is the one rejection that does not become a 422, because the
// caller can act on it. The UI branches on the code, so collapsing it back into
// CodeRejected would silently remove the re-enroll prompt.
func TestMapUpdateError_DeactivatedIs409(t *testing.T) {
	err := temporal.NewNonRetryableApplicationError(
		"customer is deactivated; re-enroll them before adding points",
		rewards.ErrTypeDeactivated, nil)

	mapped := mapUpdateError(err)
	gotCode, gotKind := status(t, mapped)
	if gotCode != http.StatusConflict || gotKind != CodeDeactivated {
		t.Fatalf("got %d/%s, want 409/%s", gotCode, gotKind, CodeDeactivated)
	}

	// Still the workflow's own words, like every other business rejection.
	var apiErr *apiError
	errors.As(mapped, &apiErr)
	if apiErr.message != "customer is deactivated; re-enroll them before adding points" {
		t.Errorf("message = %q, want the workflow's own words", apiErr.message)
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

// A rollover and a deactivation produce the identical NotFound. isClosedRun only
// detects that much; deciding which requires asking the server what is running.
func TestIsClosedRun(t *testing.T) {
	if isClosedRun(nil) {
		t.Error("nil is not a closed run")
	}
	// A rejection is the workflow answering, not the run disappearing.
	if isClosedRun(temporal.NewApplicationError("reason is required", "")) {
		t.Error("a business rejection must not be treated as a closed run")
	}
	// Observed for both a rolled-over run and a deactivated customer.
	if !isClosedRun(serviceerror.NewNotFound("workflow execution already completed")) {
		t.Error("expected NotFound to be recognised as a closed run")
	}
	// An untyped error carrying similar words is not evidence of anything --
	// matching on the message is what sent "please retry" to callers adding
	// points to a departed customer.
	if isClosedRun(errors.New("workflow execution already completed")) {
		t.Error("message text alone must not classify a closed run")
	}
}

// Whatever the wording, the status stays 503: these are transient server-side
// conditions, not caller mistakes.
func TestUnrelatedFailedPreconditionIsStill503(t *testing.T) {
	err := serviceerror.NewFailedPrecondition("namespace is in a bad state")
	gotCode, gotKind := status(t, mapQueryError(err))
	if gotCode != http.StatusServiceUnavailable || gotKind != CodeWorkerUnavailable {
		t.Errorf("got %d/%s, want 503/%s", gotCode, gotKind, CodeWorkerUnavailable)
	}
}

// The four answers GetWorkflowHistory gives, transcribed from a real server. The
// audit crawl detects truncation by this classification, so a wrong answer turns
// a truncated timeline into a 500.
//
// "history was deleted" and "you invented a run ID" are byte-identical. That is
// tolerable only because the crawl exclusively passes run IDs the server itself
// produced in a ContinuedExecutionRunId.
func TestIsHistoryGone(t *testing.T) {
	const reaped = "Requested workflow history not found, may have passed retention period."

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		// Same message whether the run aged out or `make reap` deleted it.
		{"run reaped", serviceerror.NewInvalidArgument(reaped), true},
		{"workflow never existed", serviceerror.NewNotFound("workflow not found for ID: customer-x"), true},

		// Also InvalidArgument, and emphatically not truncation: a malformed run
		// ID is a bug in the caller, and swallowing it as "history deleted" would
		// serve a short timeline instead of reporting the fault.
		{"malformed run id", serviceerror.NewInvalidArgument("Invalid RunId."), false},

		{"no worker", serviceerror.NewFailedPrecondition("no poller seen for task queue recently"), false},
		{"transport", serviceerror.NewUnavailable("connection refused"), false},
		{"nil", nil, false},
	} {
		if got := isHistoryGone(tc.err); got != tc.want {
			t.Errorf("%s: isHistoryGone(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// A timeout on a worker-free read is the contract's single 503, whether the
// deadline was ours or the server's, wrapped in transit or not.
func TestMapStoreReadError_TimeoutIs503(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		serviceerror.NewDeadlineExceeded("context deadline exceeded"),
		fmt.Errorf("reading history: %w", context.DeadlineExceeded), // wrapped in transit
	} {
		gotCode, gotKind := status(t, mapStoreReadError(err))
		if gotCode != http.StatusServiceUnavailable || gotKind != CodeWorkerUnavailable {
			t.Errorf("got %d/%s, want 503/%s", gotCode, gotKind, CodeWorkerUnavailable)
		}
	}
}

// Everything that is not a timeout still goes through the shared classifier, so
// this mapper adds a case rather than replacing one.
func TestMapStoreReadError_FallsThroughToCommon(t *testing.T) {
	gotCode, gotKind := status(t, mapStoreReadError(
		serviceerror.NewNotFound("workflow not found")))
	if gotCode != http.StatusNotFound || gotKind != CodeNotFound {
		t.Errorf("got %d/%s, want 404/%s", gotCode, gotKind, CodeNotFound)
	}

	// A genuinely missing worker still says so -- on the endpoints where that is
	// what happened. This one only fires for reads that cannot involve a worker.
	_, gotKind = status(t, mapQueryError(
		serviceerror.NewFailedPrecondition("no poller seen for task queue recently")))
	if gotKind != CodeWorkerUnavailable {
		t.Errorf("the Query path's worker diagnosis must survive, got %s", gotKind)
	}
}
