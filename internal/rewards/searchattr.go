package rewards

import "go.temporal.io/sdk/temporal"

// Typed search attribute keys. Names must match what deploy/bootstrap.sh
// registers -- a mismatch is not a compile error, it is a workflow task
// failure at the first upsert.
//
// CustomerName is registered as Text (the SDK's "String" key is the server's
// "Text"): tokenized, so word-prefix queries match. The rest are exact-match
// or numeric.
var (
	KeyCustomerName  = temporal.NewSearchAttributeKeyString("CustomerName")
	KeyRewardsLevel  = temporal.NewSearchAttributeKeyKeyword("RewardsLevel")
	KeyRewardsPoints = temporal.NewSearchAttributeKeyInt64("RewardsPoints")
	KeyEnrolledAt    = temporal.NewSearchAttributeKeyTime("RewardsEnrolledAt")
	KeyGeneration    = temporal.NewSearchAttributeKeyInt64("RewardsGeneration")
	KeyActive        = temporal.NewSearchAttributeKeyBool("RewardsActive")
)
