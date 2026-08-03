package httpapi_test

import (
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/httpapi"
)

func TestTemporalUILinks(t *testing.T) {
	// Trailing slash on the base must not double up in the links.
	ui := httpapi.NewTemporalUI("http://localhost:8080/", "rewards")

	if got, want := ui.HistoryURL("customer-ada", "run-1"),
		"http://localhost:8080/namespaces/rewards/workflows/customer-ada/run-1/history"; got != want {
		t.Errorf("historyURL = %q, want %q", got, want)
	}
	if got, want := ui.QueryURL(""),
		"http://localhost:8080/namespaces/rewards/workflows"; got != want {
		t.Errorf("queryURL(no filter) = %q, want %q", got, want)
	}
	// The query goes through URL encoding, spaces and quotes included.
	if got, want := ui.QueryURL("RewardsLevel = 'gold'"),
		"http://localhost:8080/namespaces/rewards/workflows?query=RewardsLevel+%3D+%27gold%27"; got != want {
		t.Errorf("queryURL = %q, want %q", got, want)
	}
}
