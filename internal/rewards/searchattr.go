package rewards

import "go.temporal.io/sdk/temporal"

// Typed search attribute keys. These must match the names registered by
// deploy/bootstrap.sh -- a mismatch is not a compile error, it is a workflow task
// failure at the first upsert. Keep the two lists in sync;
// FINDINGS.md#search-attributes-and-visibility is the third copy and the one
// humans read.
var (
	KeyCustomerID    = temporal.NewSearchAttributeKeyKeyword("CustomerId")
	KeyCustomerName  = temporal.NewSearchAttributeKeyString("CustomerName")
	KeyRewardsLevel  = temporal.NewSearchAttributeKeyKeyword("RewardsLevel")
	KeyRewardsPoints = temporal.NewSearchAttributeKeyInt64("RewardsPoints")
	KeyEnrolledAt    = temporal.NewSearchAttributeKeyTime("RewardsEnrolledAt")
	KeyGeneration    = temporal.NewSearchAttributeKeyInt64("RewardsGeneration")
	KeyActive        = temporal.NewSearchAttributeKeyBool("RewardsActive")
)

// Note on CustomerName: registered as Text, and the typed constructor for Text
// is NewSearchAttributeKeyString -- the SDK's "String" is the server's "Text".
// Keyword is exact-match, Text is tokenized, which is why names use Text
// (word-prefix search via STARTS_WITH -- FINDINGS.md#prefix-search-works-on-a-text-attribute-with-three-catches)
// and IDs use Keyword (exact lookup).
