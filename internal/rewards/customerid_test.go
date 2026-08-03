package rewards_test

import (
	"strings"
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

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
		// Nothing to build an ID out of; the API turns this into a 400.
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
