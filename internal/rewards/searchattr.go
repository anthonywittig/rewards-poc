package rewards

import "go.temporal.io/sdk/temporal"

// Typed search attribute keys. Names and types must match what
// deploy/bootstrap.sh registers -- a mismatch is a workflow task failure at
// the first upsert, not a compile error.
//
// CustomerName is registered as Text (tokenized, word-prefix search); the
// SDK's constructor for Text is NewSearchAttributeKeyString. IDs use Keyword
// (exact match).
var (
	// Identity
	KeyCustomerID   = temporal.NewSearchAttributeKeyKeyword("CustomerId")
	KeyCustomerName = temporal.NewSearchAttributeKeyString("CustomerName")
	// Balance
	KeyRewardsLevel  = temporal.NewSearchAttributeKeyKeyword("RewardsLevel")
	KeyRewardsPoints = temporal.NewSearchAttributeKeyInt64("RewardsPoints")
	// Membership
	KeyEnrolledAt = temporal.NewSearchAttributeKeyTime("RewardsEnrolledAt")
	KeyActive     = temporal.NewSearchAttributeKeyBool("RewardsActive")
	// Execution
	KeyRunNumber = temporal.NewSearchAttributeKeyInt64("RewardsRunNumber")
)
