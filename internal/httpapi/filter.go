package httpapi

import (
	"regexp"
	"strings"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
)

// buildListFilter turns structured GET /api/customers query params into
// visibility clauses (e.g. ?tier=gold&status=active&name=ada).
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

// nameTermSplit mirrors the Elasticsearch standard tokenizer used for Text
// fields like CustomerName: split on non-letter/digit runs, keep apostrophes
// inside a word. See
// https://www.elastic.co/docs/reference/text-analysis/analysis-standard-tokenizer
var nameTermSplit = regexp.MustCompile(`[^\p{L}\p{N}']+`)

// nameTerms splits a typed name into the lowercase terms a CustomerName prefix
// search works in.
//
// Lowercased because indexed tokens are, and STARTS_WITH is a prefix match.
func nameTerms(input string) []string {
	var terms []string
	for _, term := range nameTermSplit.Split(strings.ToLower(input), -1) {
		// Temporal's query literals do not round-trip an apostrophe -- neither
		// \' nor '' survives -- so cut each term at the first one.
		term, _, _ = strings.Cut(term, "'")
		if term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

// validTier reports whether the value names a rung of the ladder.
func validTier(tier string) bool {
	for _, name := range tierNames() {
		if tier == name {
			return true
		}
	}
	return false
}

// tierNames is the ladder in climbing order, for the tier filter and its
// error message.
func tierNames() []string {
	var names []string
	for _, t := range rewards.Ladder() {
		names = append(names, t.Level)
	}
	return names
}
