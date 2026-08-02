package rewards

import (
	"crypto/rand"
	"strings"
)

// Customer IDs are minted here, for enrollments that arrive without one.
//
// The readable half comes from the customer's name, because the ID is also the
// workflow ID (WorkflowID) and `customer-ada-lovelace-k3f9tp` is worth far more
// in the Temporal UI than a bare UUID. The random half is what makes it an ID:
// two customers named Ada Lovelace are two customers, not a conflict, and a
// name is not unique enough to key a workflow on.

const (
	// idSuffixLen random characters, so ~1e9 IDs share a name-slug before a
	// collision is likely. Enroll retries on one anyway (see enrollMinted).
	idSuffixLen = 6
	// idSlugLimit keeps the readable half from dominating the workflow ID.
	idSlugLimit = 32
	// idAlphabet is 32 characters -- a power of two, so a uniform byte maps to
	// it without modulo bias -- minus the ones that get misread aloud or in a
	// log line: l, o, 0, 1.
	idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
)

// NewCustomerID mints an unused-by-construction customer ID for name. The
// result always satisfies the ID rules the API enforces on caller-supplied
// ones: lowercase letters, digits and hyphens, no whitespace or slashes.
func NewCustomerID(name string) string {
	if slug := idSlug(name); slug != "" {
		return slug + "-" + idSuffix()
	}
	// A name with nothing ASCII in it still gets an ID, just not a readable one.
	return "c-" + idSuffix()
}

// idSlug reduces a name to the readable half: lowercase alphanumerics, runs of
// anything else collapsed to a single hyphen, no leading or trailing hyphen.
func idSlug(name string) string {
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

func idSuffix() string {
	buf := make([]byte, idSuffixLen)
	// crypto/rand.Read is documented never to return an error; it panics on a
	// failing system source instead, which is the right outcome for an ID that
	// must not be guessable-by-accident.
	rand.Read(buf)
	for i, b := range buf {
		buf[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return string(buf)
}
