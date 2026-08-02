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

// Soft deactivation moves the answer to "is this customer still enrolled?" from
// the execution status to workflow state, which touches every read in the API
// and the one write that has to tell a restore from a duplicate. Handler-level
// tests for that; the mappers are covered in classify_test.go.

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

// A soft-deactivated customer is still Running, so if the list falls back to
// ExecutionStatus every departed customer reads as active.
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
			// The whole point: Running, but the customer has left.
			name:   "running and soft-deactivated",
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

// --- Enroll: start, duplicate, restore --------------------------------------

// A free ID starts a workflow. 201, and no Update anywhere near it -- a start
// that quietly became a reactivate would restore a stranger's balance onto a
// new customer.
func TestEnroll_FreeIDStarts(t *testing.T) {
	stub := &stubTemporal{}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada","email":"ada@example.com"}`)

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
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace","email":"ada@example.com"}`)

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
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryStatus: &rewards.CustomerStatus{CustomerID: "ada-lovelace", Active: true},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace","email":"ada@example.com"}`)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	if len(stub.startIDs) != 1 {
		t.Errorf("started %v, want a single attempt on the derived ID", stub.startIDs)
	}
}

// Same derivation, so a departed customer rejoins by signing up again under the
// name they left with -- balance intact, no ID to remember.
func TestEnroll_WithoutAnIDReactivatesTheDeparted(t *testing.T) {
	stub := &stubTemporal{
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryStatus: &rewards.CustomerStatus{CustomerID: "ada-lovelace", Points: 600, Active: false},
		reactivate:  &rewards.ReactivateResult{Changed: true},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"Ada Lovelace","email":"ada@example.com"}`)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if len(stub.updates) != 1 || stub.updates[0] != rewards.UpdateReactivate {
		t.Errorf("updates = %v, want one %s", stub.updates, rewards.UpdateReactivate)
	}
}

// A name with nothing to derive an ID from is a 400, not a workflow started
// under some invented ID the caller has no way to predict.
func TestEnroll_UnslugableNameIsA400(t *testing.T) {
	stub := &stubTemporal{}
	code, body := postEnroll(t, newTestServer(stub), `{"name":"!!!","email":"ada@example.com"}`)

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

// The ID is taken and the customer is active. That is a duplicate signup, and it
// must not reach the reactivate Update -- which would overwrite a live
// customer's name and email with the second signup's.
func TestEnroll_ActiveDuplicateIs409AndSendsNoUpdate(t *testing.T) {
	stub := &stubTemporal{
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryStatus: &rewards.CustomerStatus{CustomerID: "ada", Active: true},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Mallory","email":"mallory@example.com"}`)

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

// Re-enrolling a soft-deactivated ID reactivates in place. 200 rather than 201,
// because nothing was created.
func TestEnroll_DeactivatedIDReactivates(t *testing.T) {
	stub := &stubTemporal{
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryStatus: &rewards.CustomerStatus{CustomerID: "ada", Points: 600, Active: false},
		reactivate:  &rewards.ReactivateResult{Changed: true},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada","email":"ada@example.com"}`)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if len(stub.updates) != 1 || stub.updates[0] != rewards.UpdateReactivate {
		t.Errorf("updates = %v, want one %s", stub.updates, rewards.UpdateReactivate)
	}
	if !strings.Contains(body, commonExecution.RunId) {
		t.Errorf("response should carry the current run ID, got %s", body)
	}
}

// The customer went active between the status check and the Update. The handler
// reports Changed=false rather than applying our details over theirs, and that
// is still a duplicate enrollment -- not a successful restore.
func TestEnroll_LostRaceToAConcurrentEnrollIs409(t *testing.T) {
	stub := &stubTemporal{
		startErr:    &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryStatus: &rewards.CustomerStatus{CustomerID: "ada", Active: false},
		reactivate:  &rewards.ReactivateResult{Changed: false},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada","email":"ada@example.com"}`)

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, CodeAlreadyExists) {
		t.Errorf("code should be %q, got %s", CodeAlreadyExists, body)
	}
}

// Telling an active duplicate from a soft-deactivated vacancy is the Query's job
// and no other read's, so with the worker down that question has no answer and
// the request is a 503.
//
// The assertion that matters is the second one. Enroll reactivates on a "not
// active", so anything that answers this from a laggy read can rewrite a live
// customer's name and email; failing cannot. Visibility plainly saying "active"
// is the strongest case for guessing, which is why it is the one stubbed.
func TestEnroll_DuplicateNeedsAWorkerAndSendsNoUpdateWithoutOne(t *testing.T) {
	stub := &stubTemporal{
		startErr: &serviceerror.WorkflowExecutionAlreadyStarted{},
		queryErr: serviceerror.NewFailedPrecondition("no poller seen for task queue recently"),
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution:        &commonExecution,
			Status:           enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			SearchAttributes: searchAttrs(t, ptr(true)),
		},
	}
	code, body := postEnroll(t, newTestServer(stub), `{"customerId":"ada","name":"Ada","email":"ada@example.com"}`)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", code, body)
	}
	if stub.updates != nil {
		t.Errorf("reactivated without being able to read the customer: %v", stub.updates)
	}
}

// --- Deactivate -------------------------------------------------------------

// DELETE is the deactivate Update now, not CancelWorkflow. Repeating it stays a
// 204: the handler answers Changed=false and the API has nothing to add.
func TestDeactivate_IsAnUpdateAndRepeatsCleanly(t *testing.T) {
	stub := &stubTemporal{deactivate: &rewards.DeactivateResult{Changed: false}}
	h := newTestServer(stub)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/customers/ada", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("call %d: status = %d, want 204: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(stub.updates) != 2 {
		t.Fatalf("updates = %v, want two", stub.updates)
	}
	for _, name := range stub.updates {
		if name != rewards.UpdateDeactivate {
			t.Errorf("sent %q, want %q", name, rewards.UpdateDeactivate)
		}
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
