package httpapi

import (
	"testing"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// The degraded read path derives tiers under the *run's* ladder, not today's.
//
// fillFromSearchAttributes is what a customer sees when no worker can answer a
// Query -- a closed execution, or a soft-deactivated one during an outage. It
// has no workflow to ask which thresholds that run uses, so it reads the answer
// out of the run's own TemporalChangeVersion
// (FINDINGS.md#versioning-the-tier-thresholds).
//
// Getting this wrong is invisible in the happy path and wrong only where nobody
// looks: a pre-marker customer at 460 points would be told they are 490 away
// from platinum, a promotion their run will never make.
func TestFillFromSearchAttributes_UsesTheRunsOwnLadder(t *testing.T) {
	dc := converter.GetDefaultDataConverter()
	attrs := func(points int, changeVersions []string) *commonpb.SearchAttributes {
		t.Helper()
		encode := func(v any) *commonpb.Payload {
			p, err := dc.ToPayload(v)
			if err != nil {
				t.Fatalf("encode search attribute: %v", err)
			}
			return p
		}
		fields := map[string]*commonpb.Payload{
			rewards.KeyCustomerID.GetName():    encode("ada"),
			rewards.KeyRewardsPoints.GetName(): encode(int64(points)),
			rewards.KeyRewardsLevel.GetName():  encode(rewards.TiersV2.Level(points)),
		}
		if changeVersions != nil {
			fields[rewards.KeyChangeVersion] = encode(changeVersions)
		}
		return &commonpb.SearchAttributes{IndexedFields: fields}
	}

	for _, tc := range []struct {
		name           string
		points         int
		changeVersions []string
		wantNextTierAt int
		wantTierFloor  int
	}{
		{
			// The case the versioning exists for: 460 is gold on the new ladder
			// and basic on the old one, and this run never picked up the marker.
			name:           "pre-marker run stays on the original thresholds",
			points:         460,
			changeVersions: nil,
			wantNextTierAt: rewards.GoldThreshold,
			wantTierFloor:  0,
		},
		{
			name:           "run carrying the marker gets the lowered thresholds",
			points:         460,
			changeVersions: []string{rewards.ChangeVersionTierThresholds},
			wantNextTierAt: rewards.PlatinumThresholdV2,
			wantTierFloor:  rewards.GoldThresholdV2,
		},
		{
			// A run that made some other GetVersion decision is still a
			// pre-marker run as far as the ladder is concerned.
			name:           "an unrelated change version does not imply this one",
			points:         460,
			changeVersions: []string{"tier-notifications-1"},
			wantNextTierAt: rewards.GoldThreshold,
			wantTierFloor:  0,
		},
		{
			name:           "top of the lowered ladder has no next threshold",
			points:         960,
			changeVersions: []string{rewards.ChangeVersionTierThresholds},
			wantNextTierAt: 0,
			wantTierFloor:  rewards.PlatinumThresholdV2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out CustomerResponse
			fillFromSearchAttributes(&out, attrs(tc.points, tc.changeVersions))

			if out.Points != tc.points {
				t.Fatalf("points = %d, want %d (the fixture did not decode)", out.Points, tc.points)
			}
			if out.NextTierAt != tc.wantNextTierAt {
				t.Errorf("nextTierAt = %d, want %d", out.NextTierAt, tc.wantNextTierAt)
			}
			if out.TierFloor != tc.wantTierFloor {
				t.Errorf("tierFloor = %d, want %d", out.TierFloor, tc.wantTierFloor)
			}
		})
	}
}
