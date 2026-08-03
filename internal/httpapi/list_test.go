package httpapi

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"go.temporal.io/api/serviceerror"
)

// The list endpoint scopes every query by workflow type, so if the constant and
// the name the SDK registers ever diverge, the list silently returns an empty
// page with no error.
func TestWorkflowTypeNameMatchesRegistration(t *testing.T) {
	fn := runtime.FuncForPC(reflect.ValueOf(workflows.CustomerRewardsWorkflow).Pointer()).Name()
	registered := fn
	if i := strings.LastIndex(fn, "."); i >= 0 {
		registered = fn[i+1:]
	}
	if registered != rewards.WorkflowTypeName {
		t.Errorf("WorkflowTypeName = %q but the SDK would register %q (from %q)",
			rewards.WorkflowTypeName, registered, fn)
	}
}

func TestScopedQuery(t *testing.T) {
	scope := "WorkflowType = '" + rewards.WorkflowTypeName + "'" +
		" AND ExecutionStatus != 'ContinuedAsNew'"

	if got, want := scopedQuery(""), scope; got != want {
		t.Errorf("empty query = %q, want %q", got, want)
	}

	// The caller's filter is parenthesised. Without that, a top-level OR would
	// escape the scope: "scope AND a OR b" binds as "(scope AND a) OR b", which
	// would return every workflow matching b regardless of type.
	got := scopedQuery("RewardsLevel = 'gold' OR RewardsLevel = 'platinum'")
	want := scope + " AND (RewardsLevel = 'gold' OR RewardsLevel = 'platinum')"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.Contains(got, "AND (") {
		t.Error("caller's filter must be parenthesised or an OR escapes the type scope")
	}
}

// Visibility holds one document per Run, so without excluding ContinuedAsNew a
// customer appears once per generation. Pins the clause so it cannot be dropped
// as redundant-looking noise.
func TestScopedQueryExcludesRolledOverGenerations(t *testing.T) {
	for _, q := range []string{"", "RewardsLevel = 'gold'"} {
		got := scopedQuery(q)
		if !strings.Contains(got, "ExecutionStatus != 'ContinuedAsNew'") {
			t.Errorf("scopedQuery(%q) = %q, must exclude rolled-over generations", q, got)
		}
	}
}

// Every query the list sends is one the server built from validated params, so
// a rejection from the visibility store is our bug: a 500, never a 400 blaming
// a caller who only picked from known values.
func TestRejectedListQueryIsOurBug(t *testing.T) {
	stub := &stubTemporal{
		listErr: serviceerror.NewInvalidArgument("invalid search attribute: NoSuchAttribute"),
	}
	code, body := doGET(t, newTestServer(stub), "/api/customers?tier=gold")
	if code != 500 || body.Error.Code != CodeInternal {
		t.Errorf("got %d/%s, want 500/%s", code, body.Error.Code, CodeInternal)
	}
}
