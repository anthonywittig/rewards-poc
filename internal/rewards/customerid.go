package rewards

import "strings"

// idSlugLimit keeps the readable half from dominating the workflow ID.
const idSlugLimit = 32

// CustomerIDForName derives a customer ID from a name: lowercase letters and
// digits, runs of anything else collapsed to a single hyphen, truncated to
// idSlugLimit. "Ada Lovelace" becomes "ada-lovelace", so the workflow ID is
// "customer-ada-lovelace" -- readable in the Temporal UI.
//
// Nothing random goes in, so the derivation is also the identity rule: a
// second enrollment under the same name lands on the same workflow ID, which
// the enroll endpoint treats as a duplicate or a rejoin.
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
