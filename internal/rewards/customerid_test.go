package rewards_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// --- Minted customer IDs ----------------------------------------------------
//
// A minted ID becomes a workflow ID and a URL path segment, so the shape is not
// cosmetic: the API rejects a customer ID with whitespace or a slash in it, and
// ValidateEnrollment fails a run whose ID does not match its payload.

var mintedID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestNewCustomerID_ShapeSurvivesAnyName(t *testing.T) {
	for _, name := range []string{
		"Ada Lovelace",
		"  Ada  Lovelace  ",
		"O'Brien-Smith, Jr.",
		"ada@example.com",
		"Ада Лавлейс", // nothing ASCII to slug
		"",            // the form requires a name; the API does not
		"!!!",
		strings.Repeat("Wolfeschlegelsteinhausenbergerdorff ", 4),
	} {
		id := rewards.NewCustomerID(name)
		if !mintedID.MatchString(id) {
			t.Errorf("NewCustomerID(%q) = %q, want lowercase alphanumerics and single hyphens", name, id)
		}
		if len(id) > 48 {
			t.Errorf("NewCustomerID(%q) = %q, %d chars is longer than a workflow ID wants to be",
				name, id, len(id))
		}
	}
}

// The readable half is the point of slugging rather than minting a UUID: a
// workflow ID in the Temporal UI should say who it belongs to.
func TestNewCustomerID_KeepsTheNameReadable(t *testing.T) {
	id := rewards.NewCustomerID("Ada Lovelace")
	if !strings.HasPrefix(id, "ada-lovelace-") {
		t.Errorf("NewCustomerID(\"Ada Lovelace\") = %q, want an ada-lovelace-* slug", id)
	}
	// A name with no usable characters still gets an ID, just an opaque one.
	if id := rewards.NewCustomerID("!!!"); !strings.HasPrefix(id, "c-") {
		t.Errorf("NewCustomerID(%q) = %q, want the c-* fallback", "!!!", id)
	}
}

// The name is not an identity. Repeats have to differ, or the second customer
// named Ada Lovelace collides with the first instead of enrolling.
func TestNewCustomerID_RepeatsDiffer(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := rewards.NewCustomerID("Ada Lovelace")
		if seen[id] {
			t.Fatalf("minted %q twice in 100 tries", id)
		}
		seen[id] = true
	}
}

// A minted ID has to satisfy the same enrollment validator a hand-written one
// does -- the workflow refuses a payload that does not match its workflow ID.
func TestNewCustomerID_PassesEnrollmentValidation(t *testing.T) {
	id := rewards.NewCustomerID("Ada Lovelace")
	state := &rewards.CustomerState{CustomerID: id, Name: "Ada Lovelace", Email: "ada@example.com"}
	if err := rewards.ValidateEnrollment(rewards.WorkflowID(id), state); err != nil {
		t.Errorf("ValidateEnrollment(%q) = %v, want nil", id, err)
	}
}
