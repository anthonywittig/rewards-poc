package rewards

import "go.temporal.io/sdk/temporal"

// Typed search attribute keys. These must match the names registered by
// deploy/bootstrap.sh -- a mismatch is not a compile error, it is a workflow
// task failure at the first upsert. Keep the two lists in sync; PLAN.md 4 is
// the third copy and the one humans read.
var (
	KeyCustomerID    = temporal.NewSearchAttributeKeyKeyword("CustomerId")
	KeyCustomerEmail = temporal.NewSearchAttributeKeyKeyword("CustomerEmail")
	KeyCustomerName  = temporal.NewSearchAttributeKeyString("CustomerName")
	KeyRewardsLevel  = temporal.NewSearchAttributeKeyKeyword("RewardsLevel")
	KeyRewardsPoints = temporal.NewSearchAttributeKeyInt64("RewardsPoints")
	KeyEnrolledAt    = temporal.NewSearchAttributeKeyTime("RewardsEnrolledAt")
	KeyGeneration    = temporal.NewSearchAttributeKeyInt64("RewardsGeneration")
)

// Note on CustomerName: registered as Text, and the typed constructor for Text
// is NewSearchAttributeKeyString -- the SDK's "String" is the server's "Text".
// Keyword is exact-match, Text is tokenized, which is why names use Text
// (partial search) and IDs/emails use Keyword (exact lookup).
