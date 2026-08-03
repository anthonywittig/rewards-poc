package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// commonExecution is the execution Describe hands back so the crawl has a run ID
// to start from. Its contents do not matter; that it is non-nil does.
var commonExecution = commonpb.WorkflowExecution{
	WorkflowId: "customer-ada", RunId: "run-newest",
}

// Handler-level tests, driven through the real mux with a stubbed Temporal
// client. The mappers are covered in isolation elsewhere, which is not enough on
// its own: a timeout can be misattributed by *which mapper the handler calls*,
// so reverting either call site to mapQueryError has to fail something here.

// stubTemporal implements only the methods the endpoints under test reach.
// Embedding the interface means anything else panics rather than silently
// returning a zero value.
type stubTemporal struct {
	client.Client
	describeErr error
	historyErr  error
	listErr     error
	countErr    error

	// Membership surface (membership_test.go), all opt-in: a zero stubTemporal
	// behaves as though none of it existed.
	describeInfo *workflowpb.WorkflowExecutionInfo
	executions   []*workflowpb.WorkflowExecutionInfo
	startErr     error
	queryStatus  *rewards.CustomerStatus
	queryErr     error
	deactivate   *rewards.DeactivateResult
	updateErr    error

	// Workflow IDs passed to ExecuteWorkflow, in order.
	startIDs []string

	// Queries passed to ListWorkflow, in order. Nil means the visibility
	// store was never reached -- the assertion for rejected filter params.
	listQueries []string

	// Update names sent, in order. Nil means none -- which is the assertion
	// for the paths that must never reach the workflow at all.
	updates []string
}

func (s *stubTemporal) DescribeWorkflowExecution(
	context.Context, string, string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if s.describeErr != nil {
		return nil, s.describeErr
	}
	info := s.describeInfo
	if info == nil {
		info = &workflowpb.WorkflowExecutionInfo{Execution: &commonExecution}
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: info}, nil
}

func (s *stubTemporal) ExecuteWorkflow(
	_ context.Context, opts client.StartWorkflowOptions, _ any, _ ...any,
) (client.WorkflowRun, error) {
	s.startIDs = append(s.startIDs, opts.ID)
	if s.startErr != nil {
		return nil, s.startErr
	}
	return &stubRun{id: opts.ID, runID: commonExecution.RunId}, nil
}

func (s *stubTemporal) QueryWorkflow(
	context.Context, string, string, string, ...any,
) (converter.EncodedValue, error) {
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	if s.queryStatus == nil {
		return nil, serviceerror.NewNotFound("workflow not found")
	}
	return &stubEncoded{value: *s.queryStatus}, nil
}

// stubEncoded is a Query result that round-trips through the real converter, so
// a status type that does not encode fails here rather than decoding to a
// zero-valued, and therefore inactive, customer.
type stubEncoded struct{ value any }

func (e *stubEncoded) HasValue() bool { return e.value != nil }
func (e *stubEncoded) Get(out any) error {
	dc := converter.GetDefaultDataConverter()
	p, err := dc.ToPayloads(e.value)
	if err != nil {
		return err
	}
	return dc.FromPayloads(p, out)
}

func (s *stubTemporal) UpdateWorkflow(
	_ context.Context, opts client.UpdateWorkflowOptions,
) (client.WorkflowUpdateHandle, error) {
	s.updates = append(s.updates, opts.UpdateName)
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	if opts.UpdateName == rewards.UpdateDeactivate {
		if s.deactivate == nil {
			return &stubHandle{result: rewards.DeactivateResult{Changed: true}}, nil
		}
		return &stubHandle{result: *s.deactivate}, nil
	}
	return nil, serviceerror.NewNotFound("no stub for update " + opts.UpdateName)
}

func (s *stubTemporal) GetWorkflowHistory(
	context.Context, string, string, bool, enumspb.HistoryEventFilterType,
) client.HistoryEventIterator {
	return &stubIterator{err: s.historyErr}
}

func (s *stubTemporal) ListWorkflow(
	_ context.Context, req *workflowservice.ListWorkflowExecutionsRequest,
) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	s.listQueries = append(s.listQueries, req.GetQuery())
	if s.listErr != nil {
		return nil, s.listErr
	}
	return &workflowservice.ListWorkflowExecutionsResponse{Executions: s.executions}, nil
}

func (s *stubTemporal) CountWorkflow(
	context.Context, *workflowservice.CountWorkflowExecutionsRequest,
) (*workflowservice.CountWorkflowExecutionsResponse, error) {
	if s.countErr != nil {
		return nil, s.countErr
	}
	return &workflowservice.CountWorkflowExecutionsResponse{Count: int64(len(s.executions))}, nil
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

// A store read that runs out of time is a 503 under the contract's single
// retryable code. These go through the full mux so that a handler reverting to
// mapQueryError -- which blames a worker these endpoints never speak to --
// still has its status and code pinned here.
func TestStoreReadTimeoutsAre503(t *testing.T) {
	cases := []struct {
		name string
		stub *stubTemporal
		path string
	}{
		// Describe succeeds, so the failure lands in the crawl itself.
		{"audit crawl", &stubTemporal{historyErr: context.DeadlineExceeded},
			"/api/customers/ada/audit"},
		// The Describe that bootstraps the crawl is a server call like any other.
		{"audit describe", &stubTemporal{describeErr: context.DeadlineExceeded},
			"/api/customers/ada/audit"},
		// The list reads the visibility index and is equally worker-free.
		{"customer list", &stubTemporal{
			countErr: context.DeadlineExceeded,
			listErr:  context.DeadlineExceeded,
		}, "/api/customers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doGET(t, newTestServer(tc.stub), tc.path)
			if code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", code)
			}
			if body.Error.Code != CodeWorkerUnavailable {
				t.Errorf("code = %q, want %q", body.Error.Code, CodeWorkerUnavailable)
			}
		})
	}
}
