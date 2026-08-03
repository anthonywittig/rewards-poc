package httpapi_test

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/httpapi"
)

// nameTerms mirrors Elasticsearch's standard tokenizer, because that is what
// indexed the CustomerName field the terms are matched against: an intra-word
// apostrophe does not split, everything else non-alphanumeric does.
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
		if got := httpapi.NameTerms(tc.input); !reflect.DeepEqual(got, tc.want) {
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
			got, err := httpapi.BuildListFilter(tc.tier, tc.status, tc.q)
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
			_, err := httpapi.BuildListFilter(tc.tier, tc.status, "")
			var apiErr *httpapi.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want an apiError", err)
			}
			if apiErr.Status() != http.StatusBadRequest || apiErr.Code() != httpapi.CodeInvalidRequest {
				t.Errorf("got %d/%s, want 400/%s", apiErr.Status(), apiErr.Code(), httpapi.CodeInvalidRequest)
			}
		})
	}
}
