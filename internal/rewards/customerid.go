package rewards

import "strings"

// Customer IDs are derived here, for enrollments that arrive without one.
//
// The ID is the name in lowercase with the gaps hyphenated -- "Ada Lovelace"
// becomes "ada-lovelace", and the workflow ID (WorkflowID) becomes
// "customer-ada-lovelace", which is worth far more in the Temporal UI than a
// bare UUID.
//
// Nothing random goes into it, so the derivation is the identity rule too: a
// second enrollment under the same name lands on the same workflow ID, which
// the enroll handler already treats as a duplicate (409) or a rejoin. Two
// people with one name are one customer here.

// idSlugLimit keeps the readable half from dominating the workflow ID.
const idSlugLimit = 32

// CustomerIDForName derives the customer ID for name: lowercase letters and
// digits, runs of anything else collapsed to a single hyphen, truncated to
// idSlugLimit. The result satisfies the ID rules the API enforces on
// caller-supplied IDs -- no whitespace, no slashes.
//
// Returns "" when the name has nothing to build an ID from (empty, or written
// entirely in a script this reduces away). Callers decide what that means;
// there is no ID to invent for it.
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
