package rewards_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// --- Customer IDs derived from names ----------------------------------------
//
// A derived ID becomes a workflow ID and a URL path segment, so the shape is
// not cosmetic: the API rejects a customer ID with whitespace or a slash in it,
// and ValidateEnrollment fails a run whose ID does not match its payload.

var derivedID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestCustomerIDForName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"Ada Lovelace", "ada-lovelace"},
		{"  Ada  Lovelace  ", "ada-lovelace"},
		{"ADA LOVELACE", "ada-lovelace"},
		{"O'Brien-Smith, Jr.", "o-brien-smith-jr"},
		{"ada+lovelace@work", "ada-lovelace-work"},
		{"C3PO", "c3po"},
		// Nothing to build an ID out of. The handler turns this into a 400
		// rather than inventing one.
		{"", ""},
		{"!!!", ""},
		{"Ада Лавлейс", ""},
		// Truncated, not rejected: a workflow ID should stay readable.
		{strings.Repeat("Wolfeschlegelstein ", 4), "wolfeschlegelstein-wolfeschlegel"},
	} {
		if got := rewards.CustomerIDForName(tc.name); got != tc.want {
			t.Errorf("CustomerIDForName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Whatever comes out has to be usable as a workflow ID and a path segment.
func TestCustomerIDForName_ShapeSurvivesAnyName(t *testing.T) {
	for _, name := range []string{
		"Ada Lovelace", "  Ada  Lovelace  ", "O'Brien-Smith, Jr.", "ada+lovelace@work",
		"C3PO", "-- dashes --", "半角/全角", strings.Repeat("Wolfeschlegelstein ", 4),
	} {
		id := rewards.CustomerIDForName(name)
		if id == "" {
			continue
		}
		if !derivedID.MatchString(id) {
			t.Errorf("CustomerIDForName(%q) = %q, want lowercase alphanumerics and single hyphens",
				name, id)
		}
		if len(id) > idSlugLimitForTest {
			t.Errorf("CustomerIDForName(%q) = %q, %d chars is longer than the cap", name, id, len(id))
		}
	}
}

// The derivation *is* the identity rule -- the enroll handler leans on the same
// name landing on the same workflow ID, where it becomes a duplicate or a
// rejoin rather than a second customer.
func TestCustomerIDForName_IsStable(t *testing.T) {
	first := rewards.CustomerIDForName("Ada Lovelace")
	for range 10 {
		if got := rewards.CustomerIDForName("Ada Lovelace"); got != first {
			t.Fatalf("CustomerIDForName is not deterministic: %q then %q", first, got)
		}
	}
}

// A derived ID has to satisfy the same enrollment validator a hand-written one
// does -- the workflow refuses a payload that does not match its workflow ID.
func TestCustomerIDForName_PassesEnrollmentValidation(t *testing.T) {
	id := rewards.CustomerIDForName("Ada Lovelace")
	state := &rewards.CustomerState{CustomerID: id, Name: "Ada Lovelace"}
	if err := rewards.ValidateEnrollment(rewards.WorkflowID(id), state); err != nil {
		t.Errorf("ValidateEnrollment(%q) = %v, want nil", id, err)
	}
}

// The cap is an implementation detail of the package under test; naming it here
// keeps the assertion from hardcoding a number in two places.
const idSlugLimitForTest = 32
