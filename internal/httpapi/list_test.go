package httpapi

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/api/serviceerror"
)

// The list endpoint scopes every query by workflow type, so if the constant and
// the name the SDK actually registers ever diverge, the list silently returns
// nothing -- no error, just an empty page. Derive the registered name the same
// way the SDK does and compare.
func TestWorkflowTypeNameMatchesRegistration(t *testing.T) {
	fn := runtime.FuncForPC(reflect.ValueOf(rewards.CustomerRewardsWorkflow).Pointer()).Name()
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
	scope := "WorkflowType = '" + rewards.WorkflowTypeName + "'"

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

// ORDER BY is intercepted before the query is sent, purely so the error is
// useful: Temporal's own "ORDER BY clause is not supported" is destroyed by our
// parenthesising, which turns it into a bare syntax error.
func TestHasOrderBy(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"RewardsLevel = 'gold' ORDER BY RewardsPoints DESC", true},
		{"order by points", true},
		{"RewardsLevel = 'gold' Order By RewardsPoints", true},
		{"RewardsLevel = 'gold'", false},
		{"", false},
		// A customer actually called "order by" is searchable, not rejected.
		{"CustomerName = 'order by'", false},
		{"CustomerName = 'order by' AND RewardsLevel = 'gold'", false},
		// ...but a real clause alongside a quoted one is still caught.
		{"CustomerName = 'order by' ORDER BY RewardsPoints", true},
	} {
		if got := hasOrderBy(tc.query); got != tc.want {
			t.Errorf("hasOrderBy(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// A rejected query is the caller's fault and carries Temporal's diagnostics; a
// rejection with no caller query is ours, and must not be blamed on them.
func TestMapListError(t *testing.T) {
	invalid := newInvalidArgument("invalid search attribute: NoSuchAttribute")

	mapped := mapListError(invalid, "NoSuchAttribute = 'x'")
	gotCode, gotKind := status(t, mapped)
	if gotCode != 400 || gotKind != CodeInvalidRequest {
		t.Fatalf("got %d/%s, want 400/%s", gotCode, gotKind, CodeInvalidRequest)
	}
	var apiErr *apiError
	asAPIError(mapped, &apiErr)
	if !strings.Contains(apiErr.message, "NoSuchAttribute") {
		t.Errorf("server diagnostics should reach the caller, got %q", apiErr.message)
	}

	// Our own scoping clause was rejected: a 400 would blame the wrong party.
	gotCode, _ = status(t, mapListError(invalid, ""))
	if gotCode != 500 {
		t.Errorf("rejection with no caller query = %d, want 500", gotCode)
	}

	// Anything not a rejected query is left for the common classifier.
	if mapListError(newUnavailable("connection refused"), "q") != nil {
		t.Error("non-InvalidArgument must fall through to the common classifier")
	}
}

// Small helpers so the test reads as intent rather than as SDK plumbing.
func newInvalidArgument(msg string) error  { return serviceerror.NewInvalidArgument(msg) }
func newUnavailable(msg string) error      { return serviceerror.NewUnavailable(msg) }
func asAPIError(err error, dst **apiError) { errors.As(err, dst) }
