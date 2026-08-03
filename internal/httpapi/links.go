package httpapi

import (
	"net/url"
	"strings"
)

// Deep links into the Temporal UI, built server-side: the API knows its
// namespace and is told where the UI lives (TEMPORAL_UI_URL), so the client
// carries no Temporal configuration -- previously two env vars on the web
// service that had to be kept in sync with the api and worker by hand.

// temporalUI builds the links. Zero value yields relative-ish garbage, so the
// server always constructs one via newTemporalUI.
type temporalUI struct {
	base      string // no trailing slash
	namespace string
}

func newTemporalUI(baseURL, namespace string) temporalUI {
	return temporalUI{base: strings.TrimRight(baseURL, "/"), namespace: namespace}
}

// historyURL deep-links one run's Event History.
func (t temporalUI) historyURL(workflowID, runID string) string {
	return t.base + "/namespaces/" + url.PathEscape(t.namespace) +
		"/workflows/" + url.PathEscape(workflowID) +
		"/" + url.PathEscape(runID) + "/history"
}

// queryURL opens the workflow list, pre-filled with the visibility query when
// there is one.
func (t temporalUI) queryURL(query string) string {
	u := t.base + "/namespaces/" + url.PathEscape(t.namespace) + "/workflows"
	if query == "" {
		return u
	}
	return u + "?query=" + url.QueryEscape(query)
}
