package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// commonExecution is the execution Describe hands back so the crawl has a run ID
// to start from. Its contents do not matter; that it is non-nil does.
var commonExecution = commonpb.WorkflowExecution{
	WorkflowId: "customer-ada", RunId: "run-newest",
}

// Handler-level tests, driven through the real mux with a stubbed Temporal
// client. Everything else in this package tests a mapper or a pure function in
// isolation, which turned out not to be enough: the timeout misattribution
// reported on PR #13 lived in *which mapper the handler called*, so a suite that
// only exercised the mappers passed with the bug fully intact. Reverting either
// call site to mapQueryError has to fail something, and now does.

// stubTemporal implements only the methods the endpoints under test reach.
// Embedding the interface means anything else panics rather than silently
// returning a zero value -- a test that wandered off the intended path should
// fail loudly rather than assert on nothing.
type stubTemporal struct {
	client.Client
	describeErr error
	historyErr  error
	listErr     error
	countErr    error
	startErr    error
	// Status Describe reports. Zero value is UNSPECIFIED, which is neither
	// running nor any particular closure -- fine for the crawl tests, which only
	// need the execution to exist, and set explicitly by the enroll tests, which
	// turn on exactly this field.
	describeStatus enumspb.WorkflowExecutionStatus
}

func (s *stubTemporal) DescribeWorkflowExecution(
	context.Context, string, string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if s.describeErr != nil {
		return nil, s.describeErr
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonExecution,
			Status:    s.describeStatus,
		},
	}, nil
}

func (s *stubTemporal) ExecuteWorkflow(
	context.Context, client.StartWorkflowOptions, interface{}, ...interface{},
) (client.WorkflowRun, error) {
	return nil, s.startErr
}

func (s *stubTemporal) GetWorkflowHistory(
	context.Context, string, string, bool, enumspb.HistoryEventFilterType,
) client.HistoryEventIterator {
	return &stubIterator{err: s.historyErr}
}

func (s *stubTemporal) ListWorkflow(
	context.Context, *workflowservice.ListWorkflowExecutionsRequest,
) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return nil, s.listErr
}

func (s *stubTemporal) CountWorkflow(
	context.Context, *workflowservice.CountWorkflowExecutionsRequest,
) (*workflowservice.CountWorkflowExecutionsResponse, error) {
	return nil, s.countErr
}

// stubIterator always has a next event and always fails to produce it, which is
// how a mid-crawl failure reaches walkRuns.
type stubIterator struct{ err error }

func (i *stubIterator) HasNext() bool { return true }
func (i *stubIterator) Next() (*historypb.HistoryEvent, error) {
	return nil, i.err
}

func newTestServer(stub *stubTemporal) http.Handler {
	return New(stub, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()
}

func doGET(t *testing.T, h http.Handler, path string) (int, ErrorResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var body ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s response: %v", path, err)
	}
	return rec.Code, body
}

// The reported bug, end to end. A crawl that runs out of time must not tell the
// caller to go and look at a worker it never spoke to.
func TestGetAudit_TimeoutDoesNotBlameTheWorker(t *testing.T) {
	// Describe succeeds, so the failure lands in the crawl itself -- the path
	// the report was about.
	h := newTestServer(&stubTemporal{historyErr: context.DeadlineExceeded})

	code, body := doGET(t, h, "/api/customers/ada/audit")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	assertBlamesNoWorker(t, body, auditSubject)
}

// Same requirement for the Describe that bootstraps the crawl. It is a server
// call like any other here, and no worker is involved in it either.
func TestGetAudit_DescribeTimeoutDoesNotBlameTheWorker(t *testing.T) {
	h := newTestServer(&stubTemporal{describeErr: context.DeadlineExceeded})

	code, body := doGET(t, h, "/api/customers/ada/audit")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	assertBlamesNoWorker(t, body, auditSubject)
}

// The list reads the visibility index and is equally worker-free. Not reported,
// found while fixing the audit endpoint -- the two share the defect because they
// shared the mapper.
func TestListCustomers_TimeoutDoesNotBlameTheWorker(t *testing.T) {
	h := newTestServer(&stubTemporal{
		countErr: context.DeadlineExceeded,
		listErr:  context.DeadlineExceeded,
	})

	code, body := doGET(t, h, "/api/customers")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	assertBlamesNoWorker(t, body, "the customer list")
}

// --- Enrollment conflicts (PLAN.md 3.6) -------------------------------------

func doPOST(t *testing.T, h http.Handler, path, body string) (int, ErrorResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))

	var out ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s response: %v", path, err)
	}
	return rec.Code, out
}

const enrollBody = `{"customerId":"ada","name":"Ada Lovelace","email":"ada@example.com"}`

// alreadyStarted is the one error both refusals arrive as. The message differs
// on a real server ("already running" against "already finished successfully"),
// and nothing here reads it -- which is the point: the handler resolves the
// ambiguity by asking the server what is running, not by matching text. Using
// the *same* message in both tests below is what keeps that honest.
func alreadyStarted() error {
	return serviceerror.NewWorkflowExecutionAlreadyStarted(
		"Workflow execution already started", "customer-ada", "run-1")
}

// A departed customer cannot be re-enrolled: the reuse policy refuses the ID,
// and the API says so in terms the caller can act on.
func TestEnroll_DepartedCustomerIsRefusedAsDeactivated(t *testing.T) {
	h := newTestServer(&stubTemporal{
		startErr:       alreadyStarted(),
		describeStatus: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
	})

	code, body := doPOST(t, h, "/api/customers", enrollBody)

	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
	if body.Error.Code != CodeDeactivated {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeDeactivated)
	}
	// The remedy matters more than the wording: telling someone their customer
	// is "already enrolled and active" sends them to look at a customer who
	// left, and this is the whole reason the handler spends a Describe.
	if strings.Contains(body.Error.Message, "active") {
		t.Errorf("a departed customer must not be described as active: %q", body.Error.Message)
	}
}

// The other half of the same ambiguity. An active customer still gets the
// duplicate-enrollment 409 they always did.
func TestEnroll_ActiveCustomerIsRefusedAsAlreadyEnrolled(t *testing.T) {
	h := newTestServer(&stubTemporal{
		startErr:       alreadyStarted(),
		describeStatus: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	})

	code, body := doPOST(t, h, "/api/customers", enrollBody)

	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
	if body.Error.Code != CodeAlreadyExists {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeAlreadyExists)
	}
}

// If the disambiguating Describe fails, the enrollment was still refused. Report
// that, rather than the 503 the Describe failure would otherwise produce: a
// retry cannot succeed either way, and a 503 says it might.
func TestEnroll_UndisambiguatedConflictStillReportsTheConflict(t *testing.T) {
	h := newTestServer(&stubTemporal{
		startErr:    alreadyStarted(),
		describeErr: context.DeadlineExceeded,
	})

	code, body := doPOST(t, h, "/api/customers", enrollBody)

	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
	if body.Error.Code != CodeAlreadyExists {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeAlreadyExists)
	}
}

func assertBlamesNoWorker(t *testing.T, body ErrorResponse, subject string) {
	t.Helper()
	if body.Error.Code != CodeWorkerUnavailable {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeWorkerUnavailable)
	}
	if strings.Contains(strings.ToLower(body.Error.Message), "worker") {
		t.Errorf("message names the worker on a worker-free read: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, subject) {
		t.Errorf("message should name %q, got %q", subject, body.Error.Message)
	}
}
