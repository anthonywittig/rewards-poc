package rewards

import "strings"

// The identity rule: name -> customer ID -> workflow ID, with nothing random
// at either step. The same name always lands on the same workflow, which is
// what lets every operation skip a lookup table and the enroll endpoint treat
// a repeat name as a duplicate.

// WorkflowIDPrefix makes the workflow ID derivable from the customer ID alone.
const WorkflowIDPrefix = "customer-"

// WorkflowID returns the deterministic workflow ID for a customer.
func WorkflowID(customerID string) string { return WorkflowIDPrefix + customerID }

// idSlugLimit keeps the readable half from dominating the workflow ID.
const idSlugLimit = 32

// CustomerIDForName derives a customer ID from a name: lowercase letters and
// digits, runs of anything else collapsed to a single hyphen, truncated to
// idSlugLimit. "Ada Lovelace" becomes "ada-lovelace", so the workflow ID is
// "customer-ada-lovelace" -- readable in the Temporal UI.
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
		if b.Len() >= idSlugLimit {
			break
		}
	}
	return b.String()
}
