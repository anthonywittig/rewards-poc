package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// Deactivation is one-way and completes the workflow, so "is this customer
// still enrolled?" is answered by RewardsActive for reads and by whether an
// execution is Running for the enroll conflict path. Handler-level tests for
// that; the mappers are covered in classify_test.go.

// --- The list ---------------------------------------------------------------

// searchAttrs encodes what the workflow upserts. Values are payloads because
// that is what visibility hands back.
func searchAttrs(t *testing.T, active *bool) *commonpb.SearchAttributes {
	t.Helper()
	dc := converter.GetDefaultDataConverter()

	encode := func(v any) *commonpb.Payload {
		p, err := dc.ToPayload(v)
		if err != nil {
			t.Fatalf("encode search attribute: %v", err)
		}
		return p
	}
	fields := map[string]*commonpb.Payload{
		rewards.KeyCustomerID.GetName():    encode("ada"),
		rewards.KeyCustomerName.GetName():  encode("Ada Lovelace"),
		rewards.KeyRewardsPoints.GetName(): encode(int64(600)),
		rewards.KeyRewardsLevel.GetName():  encode(rewards.LevelGold),
	}
	if active != nil {
		fields[rewards.KeyActive.GetName()] = encode(*active)
	}
	return &commonpb.SearchAttributes{IndexedFields: fields}
}

func ptr[T any](v T) *T { return &v }

// Membership is RewardsActive, not ExecutionStatus: a departed customer's
// final run is Completed but still listed, and it must read as deactivated.
func TestListCustomers_StatusComesFromRewardsActive(t *testing.T) {
	cases := []struct {
		name   string
		active *bool
		status enumspb.WorkflowExecutionStatus
		want   string
	}{
		{
			name:   "running and active",
			active: ptr(true),
			status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			want:   "active",
		},
		{
			// The departed customer: deactivation completed the run, and the
			// final run's attributes carry the leave.
			name:   "completed and deactivated",
			active: ptr(false),
			status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
			want:   "deactivated",
		},
		{
			// The drain window: the leave has been recorded but the run has not
			// closed yet. Still a departed customer.
			name:   "running and deactivated",
			active: ptr(false),
			status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			want:   "deactivated",
		},
		{
			// Enrolled before RewardsActive existed, or reaped attributes.
			// Nothing says otherwise, so ExecutionStatus decides.
			name:   "no attribute, still running",
			active: nil,
			status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			want:   "active",
		},
		{
			name:   "no attribute, closed",
			active: nil,
			status: enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED,
			want:   "deactivated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(&stubTemporal{
				executions: []*workflowpb.WorkflowExecutionInfo{{
					Execution:        &commonExecution,
					Status:           tc.status,
					SearchAttributes: searchAttrs(t, tc.active),
				}},
			})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/customers", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			var body CustomerListResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Items) != 1 {
				t.Fatalf("got %d items, want 1", len(body.Items))
			}
			if body.Items[0].Status != tc.want {
				t.Errorf("status = %q, want %q", body.Items[0].Status, tc.want)
			}
		})
	}
}

// --- Enroll: start, duplicate, departed --------------------------------------

// A free ID starts a workflow. 201, and no Update anywhere near it.
func TestEnroll_FreeIDStarts(t *testing.T) {
	stub := &stubTemporal{}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada"}`)

	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, body)
	}
	if stub.updates != nil {
		t.Errorf("a fresh enroll sent Updates: %v", stub.updates)
	}
}

// A signup sends no customerId at all: the server derives one from the name and
// answers with it. The response is the caller's only way to learn the ID, so an
// empty one there strands the customer the request just created.
func TestEnroll_WithoutAnIDDerivesOneFromTheName(t *testing.T) {
	stub := &stubTemporal{}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace"}`)

	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, body)
	}

	var res EnrollResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.CustomerID != "ada-lovelace" {
		t.Errorf("customerId = %q, want %q", res.CustomerID, "ada-lovelace")
	}
	if want := rewards.WorkflowID("ada-lovelace"); res.WorkflowID != want {
		t.Errorf("workflowId = %q, want %q", res.WorkflowID, want)
	}
	if len(stub.startIDs) != 1 || stub.startIDs[0] != rewards.WorkflowID("ada-lovelace") {
		t.Errorf("started %v, want one start on %q", stub.startIDs, rewards.WorkflowID("ada-lovelace"))
	}
}

// The derivation is the identity rule: a second signup under one name is the
// same customer, and lands on the duplicate path rather than starting a rival
// workflow. Getting this wrong is not a cosmetic ID difference -- it is two
// executions for one person.
func TestEnroll_SecondSignupUnderOneNameIsTheDuplicatePath(t *testing.T) {
	stub := &stubTemporal{
		startErr: &serviceerror.WorkflowExecutionAlreadyStarted{},
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonExecution,
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace"}`)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	if len(stub.startIDs) != 1 {
		t.Errorf("started %v, want a single attempt on the derived ID", stub.startIDs)
	}
}

// Deactivation is one-way: signing up again under a departed customer's name is
// refused with the code that says so, and nothing reaches the workflow -- there
// is no workflow left to reach.
func TestEnroll_DepartedIDIsA409Deactivated(t *testing.T) {
	stub := &stubTemporal{
		startErr: &serviceerror.WorkflowExecutionAlreadyStarted{},
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonExecution,
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace"}`)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, CodeDeactivated) {
		t.Errorf("code should be %q, got %s", CodeDeactivated, body)
	}
	if stub.updates != nil {
		t.Errorf("an enroll against a departed customer sent Updates: %v", stub.updates)
	}
}

// A name with nothing to derive an ID from is a 400, not a workflow started
// under some invented ID the caller has no way to predict.
func TestEnroll_UnslugableNameIsA400(t *testing.T) {
	stub := &stubTemporal{}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"!!!"}`)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, CodeInvalidRequest) {
		t.Errorf("code should be %q, got %s", CodeInvalidRequest, body)
	}
	if stub.startIDs != nil {
		t.Errorf("started something anyway: %v", stub.startIDs)
	}
}

// The ID is taken and the customer is active. That is a duplicate signup, and
// it must not reach the workflow -- there is nothing an Update could add.
func TestEnroll_ActiveDuplicateIs409AndSendsNoUpdate(t *testing.T) {
	stub := &stubTemporal{
		startErr: &serviceerror.WorkflowExecutionAlreadyStarted{},
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonExecution,
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Mallory"}`)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, CodeAlreadyExists) {
		t.Errorf("code should be %q, got %s", CodeAlreadyExists, body)
	}
	if stub.updates != nil {
		t.Errorf("a duplicate enroll reached the workflow: %v", stub.updates)
	}
}

// Telling an active duplicate from a departed customer takes one Describe, and
// with no answer there is no answer: the request fails rather than guessing at
// which 409 to hand back.
func TestEnroll_DuplicateWithNoDescribeAnswerIsA503(t *testing.T) {
	stub := &stubTemporal{
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		describeErr: context.DeadlineExceeded,
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada"}`)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", code, body)
	}
	// Describe reads persistence, so the failure must not blame a worker no
	// enroll ever speaks to.
	if strings.Contains(body, "make worker") {
		t.Errorf("a store read failure blamed the worker: %s", body)
	}
	if stub.updates != nil {
		t.Errorf("an unanswerable enroll reached the workflow: %v", stub.updates)
	}
}

// --- Deactivate -------------------------------------------------------------

// DELETE is the deactivate Update now, not CancelWorkflow.
func TestDeactivate_IsAnUpdate(t *testing.T) {
	stub := &stubTemporal{deactivate: &rewards.DeactivateResult{Changed: true}}
	h := newTestServer(stub)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/customers/ada", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if len(stub.updates) != 1 || stub.updates[0] != rewards.UpdateDeactivate {
		t.Fatalf("updates = %v, want one %s", stub.updates, rewards.UpdateDeactivate)
	}
}

// Deactivation completes the workflow, so a repeat DELETE finds the run closed:
// the Update comes back NotFound, the Describe says nothing is running, and
// that is exactly what deactivation leaves behind -- still a 204.
func TestDeactivate_RepeatAgainstTheClosedRunIsStillA204(t *testing.T) {
	stub := &stubTemporal{
		updateErr: serviceerror.NewNotFound("workflow execution already completed"),
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonExecution,
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		},
	}
	h := newTestServer(stub)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/customers/ada", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

func postEnroll(t *testing.T, h http.Handler, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/customers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// --- Stub plumbing ----------------------------------------------------------

// stubRun satisfies the bit of client.WorkflowRun ExecuteWorkflow's caller uses.
type stubRun struct {
	client.WorkflowRun
	id, runID string
}

func (r *stubRun) GetID() string    { return r.id }
func (r *stubRun) GetRunID() string { return r.runID }

// stubHandle is an Update handle whose result is fixed up front. Get decodes via
// the same converter the SDK uses, so a result type that does not round-trip
// fails here rather than silently zeroing.
type stubHandle struct {
	client.WorkflowUpdateHandle
	result any
}

func (h *stubHandle) Get(_ context.Context, out any) error {
	if out == nil || h.result == nil {
		return nil
	}
	dc := converter.GetDefaultDataConverter()
	p, err := dc.ToPayloads(h.result)
	if err != nil {
		return err
	}
	return dc.FromPayloads(p, out)
}

// A name is required whether or not the ID is derived from it. Sending a
// customerId is the path that would otherwise slip through -- the derivation is
// skipped, so nothing else looks at the name, and the customer is started
// nameless. The workflow refuses that too, but as a failed execution rather
// than an answer the caller can act on.
func TestEnroll_BlankNameIsA400EvenWithACustomerID(t *testing.T) {
	for _, body := range []string{
		`{"customerId":"ada","name":""}`,
		`{"customerId":"ada","name":"   "}`,
		`{"customerId":"ada"}`,
		`{"name":"   "}`,
	} {
		t.Run(body, func(t *testing.T) {
			stub := &stubTemporal{}
			code, got := postEnroll(t, newTestServer(stub), body)

			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, got)
			}
			if !strings.Contains(got, CodeInvalidRequest) {
				t.Errorf("code should be %q, got %s", CodeInvalidRequest, got)
			}
			if stub.startIDs != nil {
				t.Errorf("started a nameless customer anyway: %v", stub.startIDs)
			}
		})
	}
}
