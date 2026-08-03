package httpapi

import (
	"net/url"
	"strings"
)

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
