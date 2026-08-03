package httpapi

import (
	"regexp"
	"strings"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// Structured list filters: GET /api/customers?tier=gold&status=active&name=ada.
//
// The UI used to assemble the visibility query itself, which put the search
// attribute names, Elasticsearch's tokenization, and the no-escaping rules for
// query literals into the client. These params are the whole filtering surface
// now -- there is no raw-query param -- so all of that lives behind the API.

// buildListFilter turns the structured params into visibility query clauses.
// Every returned clause embeds only validated or sanitized values, so the
// assembled query cannot be rejected by the server -- if it is, that is our
// bug, and it surfaces as a logged 500 rather than a 400 blaming the caller.
func buildListFilter(tier, status, name string) ([]string, error) {
	var parts []string

	if tier != "" {
		if !validTier(tier) {
			return nil, badRequest("tier must be one of " + strings.Join(tierNames(), ", "))
		}
		parts = append(parts, rewards.KeyRewardsLevel.GetName()+" = '"+tier+"'")
	}

	switch status {
	case "", "any":
		// No clause: both members and former members.
	case "active":
		parts = append(parts, rewards.KeyActive.GetName()+" = true")
	case "deactivated":
		parts = append(parts, rewards.KeyActive.GetName()+" = false")
	default:
		return nil, badRequest("status must be active, deactivated, or any")
	}

	// One clause per term, ANDed, so more words narrow the match: an OR of the
	// terms would have "ada turing" return both Ada Lovelace and Alan Turing.
	//
	// No escaping: nameTerms output is letters and digits only, so each term
	// embeds in a single-quoted literal as-is.
	for _, term := range nameTerms(name) {
		parts = append(parts, rewards.KeyCustomerName.GetName()+" STARTS_WITH '"+term+"'")
	}

	return parts, nil
}

// nameTermSplit breaks on anything that is not a letter, digit, or apostrophe,
// mirroring Elasticsearch's standard tokenizer, which is what indexed the
// CustomerName field: punctuation and whitespace break tokens, but an
// intra-word apostrophe does not ("Mary-Jane" is two tokens, "O'Brien" is one).
var nameTermSplit = regexp.MustCompile(`[^\p{L}\p{N}']+`)

// nameTerms splits a typed name into the lowercase terms a CustomerName prefix
// search works in.
//
// Lowercased because indexed tokens are, and STARTS_WITH is a prefix match on
// the stored token rather than an analyzed one -- "Lovel" finds nobody.
func nameTerms(input string) []string {
	var terms []string
	for _, term := range nameTermSplit.Split(strings.ToLower(input), -1) {
		// Temporal's query literals do not round-trip an apostrophe -- neither
		// \' nor '' survives -- so cut each term at the first one. A shorter
		// prefix is still a correct prefix, it just matches more, which beats
		// "O'Brien" matching nothing at all.
		term, _, _ = strings.Cut(term, "'")
		if term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

// validTier reports whether the value names a rung of the ladder, or the floor.
func validTier(tier string) bool {
	for _, name := range tierNames() {
		if tier == name {
			return true
		}
	}
	return false
}

// tierNames is the floor plus the ladder, in climbing order, for the tier
// filter and its error message. Derived from rewards.Ladder rather than listed
// here so a new rung is filterable without touching this package.
func tierNames() []string {
	names := []string{rewards.LevelBasic}
	for _, t := range rewards.Ladder() {
		names = append(names, t.Level)
	}
	return names
}
