package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// nameTerms mirrors Elasticsearch's standard tokenizer, because that is what
// indexed the CustomerName field the terms are matched against. These cases pin
// the two behaviors that are easy to break by "simplifying" the regex: an
// intra-word apostrophe does not split, and everything else non-alphanumeric
// does.
func TestNameTerms(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"Ada Lovelace", []string{"ada", "lovelace"}},
		{"  ada   lov  ", []string{"ada", "lov"}},
		{"Mary-Jane", []string{"mary", "jane"}},
		// One token, then cut at the apostrophe: Temporal's query literals do
		// not round-trip one, and the shorter prefix still matches O'Brien.
		{"O'Brien", []string{"o"}},
		{"agent 007", []string{"agent", "007"}},
		{"", nil},
		{"  --  ", nil},
		{"'''", nil},
	} {
		if got := nameTerms(tc.input); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("nameTerms(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestBuildListFilter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tier, status, q string
		want            []string
	}{
		{"no params", "", "", "", nil},
		{"tier only", "gold", "", "", []string{"RewardsLevel = 'gold'"}},
		// The floor is filterable even though it is not a ladder rung.
		{"floor tier", "basic", "", "", []string{"RewardsLevel = 'basic'"}},
		{"active", "", "active", "", []string{"RewardsActive = true"}},
		{"deactivated", "", "deactivated", "", []string{"RewardsActive = false"}},
		{"any status is no clause", "", "any", "", nil},
		{"name terms AND so typing narrows", "", "", "ada lov",
			[]string{"CustomerName STARTS_WITH 'ada'", "CustomerName STARTS_WITH 'lov'"}},
		{"all together", "platinum", "active", "Ada", []string{
			"RewardsLevel = 'platinum'",
			"RewardsActive = true",
			"CustomerName STARTS_WITH 'ada'",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildListFilter(tc.tier, tc.status, tc.q)
			if err != nil {
				t.Fatalf("buildListFilter: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Unknown values are the caller's mistake and must say so, not silently match
// nothing -- a typo'd tier that returned an empty list would look like an
// empty program.
func TestBuildListFilterRejectsUnknownValues(t *testing.T) {
	for _, tc := range []struct{ name, tier, status string }{
		{"unknown tier", "neon", ""},
		{"unknown status", "", "asleep"},
		// Validation, not sanitization: an injection attempt is just another
		// unknown value.
		{"tier injection", "gold' OR RewardsLevel != '", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildListFilter(tc.tier, tc.status, "")
			code, kind := status(t, err)
			if code != http.StatusBadRequest || kind != CodeInvalidRequest {
				t.Errorf("got %d/%s, want 400/%s", code, kind, CodeInvalidRequest)
			}
		})
	}
}

// Through the real mux: the structured params become clauses in the query that
// actually reaches the visibility store, scoped and parenthesised.
func TestListFilterParamsReachVisibilityQuery(t *testing.T) {
	stub := &stubTemporal{}
	h := newTestServer(stub)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/customers?tier=gold&status=deactivated&name=Ada+Lov", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	if len(stub.listQueries) != 1 {
		t.Fatalf("ListWorkflow called %d times, want 1", len(stub.listQueries))
	}
	got := stub.listQueries[0]
	want := "WorkflowType = '" + rewards.WorkflowTypeName + "'" +
		" AND ExecutionStatus != 'ContinuedAsNew'" +
		" AND (RewardsLevel = 'gold'" +
		" AND RewardsActive = false" +
		" AND CustomerName STARTS_WITH 'ada'" +
		" AND CustomerName STARTS_WITH 'lov')"
	if got != want {
		t.Errorf("visibility query:\n got %q\nwant %q", got, want)
	}

	// The response echoes the effective filter -- what the UI shows as
	// pasteable into the Temporal UI.
	var res CustomerListResponse
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantEcho := "RewardsLevel = 'gold' AND RewardsActive = false" +
		" AND CustomerName STARTS_WITH 'ada' AND CustomerName STARTS_WITH 'lov'"
	if res.Query != wantEcho {
		t.Errorf("echoed query:\n got %q\nwant %q", res.Query, wantEcho)
	}
}

// A bad param fails before any visibility call: a 400 that had already fetched
// rows would look half-done.
func TestListRejectsBadFilterParamsBeforeQuerying(t *testing.T) {
	stub := &stubTemporal{}
	code, body := doGET(t, newTestServer(stub), "/api/customers?tier=neon")
	if code != http.StatusBadRequest || body.Error.Code != CodeInvalidRequest {
		t.Fatalf("got %d/%s, want 400/%s", code, body.Error.Code, CodeInvalidRequest)
	}
	if stub.listQueries != nil {
		t.Errorf("visibility store must not be queried, got %v", stub.listQueries)
	}
}
