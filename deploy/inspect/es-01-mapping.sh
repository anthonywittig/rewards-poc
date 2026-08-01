#!/usr/bin/env bash
# index mapping — custom search attributes are first-class fields.
# Run via: make inspect-es Q=mapping
# Lives in the temporal container (bash + curl, no jq).

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"
INDEX="${INDEX:-temporal_visibility_v1_dev}"

echo "=== mapping for ${INDEX} ==="
echo "(look for CustomerId, RewardsLevel, RewardsPoints, … alongside built-ins)"
echo
curl -s "${ES}/${INDEX}/_mapping?pretty"
