package rewards

import "strings"

// The identity rule: name -> customer ID -> workflow ID, with nothing random
// at either step. The same name always lands on the same workflow, which is
// what lets every operation skip a lookup table and the enroll endpoint treat
// a repeat name as a duplicate.

// WorkflowID returns the deterministic workflow ID for a customer: the
// customer ID itself, kept behind this function so the derivation stays in
// one place.
func WorkflowID(customerID string) string { return customerID }

// CustomerIDForName derives a customer ID from a name: lowercase letters and
// digits, runs of anything else collapsed to a single hyphen, truncated to
// 32 runes. "Ada Lovelace" becomes "ada-lovelace", which is also the
// workflow ID -- readable in the Temporal UI.
//
// Returns "" when the name has nothing to build an ID from.
func CustomerIDForName(name string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(name) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			pendingHyphen = b.Len() > 0
			continue
		}
		if pendingHyphen {
			b.WriteByte('-')
			pendingHyphen = false
		}
		b.WriteRune(r)
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}
