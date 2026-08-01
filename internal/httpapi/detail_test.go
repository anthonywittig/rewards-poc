package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
)

// The detail response carries the whole tier ladder, because the UI's progress
// bar needs the rung *below* the customer as well as NextTierAt and the only
// other place to get it is a hardcoded copy in the client.

func getCustomerDetail(t *testing.T, h http.Handler, id string) CustomerResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/customers/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body CustomerResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func runningExecution(sa *workflowpb.WorkflowExecutionInfo) *workflowpb.WorkflowExecutionInfo {
	if sa == nil {
		sa = &workflowpb.WorkflowExecutionInfo{}
	}
	sa.Execution = &commonExecution
	sa.Status = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	return sa
}

// The ladder is the worker's answer, passed through -- not the API's own. Those
// are separate binaries and separate deploys, so an API that substituted its
// build's thresholds could pair them with a NextTierAt from another build and
// name a target that is not on the ladder beside it.
func TestGetCustomer_LadderComesFromTheWorkerNotTheAPI(t *testing.T) {
	// Deliberately not this build's ladder: if the handler ignores what the
	// Query said, the assertion sees rewards.Ladder() instead.
	workerLadder := []rewards.Tier{
		{Level: "silver", MinPoints: 250},
		{Level: "gold", MinPoints: 750},
	}

	h := newTestServer(&stubTemporal{
		describeInfo: runningExecution(nil),
		queryStatus: &rewards.CustomerStatus{
			CustomerID: "ada",
			Points:     300,
			Level:      "silver",
			NextTierAt: 750,
			Tiers:      workerLadder,
			Active:     true,
		},
	})

	got := getCustomerDetail(t, h, "ada")
	if len(got.Tiers) != len(workerLadder) {
		t.Fatalf("tiers = %+v, want the worker's %+v", got.Tiers, workerLadder)
	}
	for i, want := range workerLadder {
		if got.Tiers[i] != want {
			t.Errorf("tiers[%d] = %+v, want %+v", i, got.Tiers[i], want)
		}
	}
}

// The degraded path serves a customer no worker can be asked about, and the
// detail page it feeds draws the same bar. Dropping the ladder here would render
// it against an empty one.
func TestGetCustomer_LadderSurvivesTheSearchAttributeFallback(t *testing.T) {
	h := newTestServer(&stubTemporal{
		describeInfo: &workflowpb.WorkflowExecutionInfo{
			Execution:        &commonExecution,
			Status:           enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED,
			SearchAttributes: searchAttrs(t, nil),
		},
		// No queryStatus: QueryWorkflow fails, which is the premise.
	})

	got := getCustomerDetail(t, h, "ada")
	if got.Status != "deactivated" {
		t.Fatalf("status = %q, want deactivated (the fallback path was not taken)", got.Status)
	}
	if len(got.Tiers) != len(rewards.Ladder()) {
		t.Errorf("tiers = %+v, want this build's ladder %+v", got.Tiers, rewards.Ladder())
	}
	// searchAttrs puts the customer at 600 points, so the bar's target is
	// platinum and its floor is a rung that has to be in what we just sent.
	if got.NextTierAt != rewards.PlatinumThreshold {
		t.Errorf("nextTierAt = %d, want %d", got.NextTierAt, rewards.PlatinumThreshold)
	}
}
