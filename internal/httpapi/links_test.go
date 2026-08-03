package httpapi

import "testing"

func TestTemporalUILinks(t *testing.T) {
	// Trailing slash on the base must not double up in the links.
	ui := newTemporalUI("http://localhost:8080/", "rewards")

	if got, want := ui.historyURL("customer-ada", "run-1"),
		"http://localhost:8080/namespaces/rewards/workflows/customer-ada/run-1/history"; got != want {
		t.Errorf("historyURL = %q, want %q", got, want)
	}
	if got, want := ui.queryURL(""),
		"http://localhost:8080/namespaces/rewards/workflows"; got != want {
		t.Errorf("queryURL(no filter) = %q, want %q", got, want)
	}
	// The query goes through URL encoding, spaces and quotes included.
	if got, want := ui.queryURL("RewardsLevel = 'gold'"),
		"http://localhost:8080/namespaces/rewards/workflows?query=RewardsLevel+%3D+%27gold%27"; got != want {
		t.Errorf("queryURL = %q, want %q", got, want)
	}
}
