#!/usr/bin/env bash
# one customer's visibility document(s).
# One ES doc per *run*, not per workflow ID — so a customer who has
# continued-as-new twice shows three hits (two ContinuedAsNew + one Running)
# until make reap removes the closed ones.
#
# Run via: make inspect-es Q=customer ID=inspect

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"
INDEX="${INDEX:-temporal_visibility_v1_dev}"
WF="${WF:-customer-inspect}"

echo "=== visibility docs for WorkflowId=${WF} ==="
echo
curl -s "${ES}/${INDEX}/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d "{\"query\":{\"term\":{\"WorkflowId\":\"${WF}\"}},\"size\":20,\"sort\":[{\"StartTime\":\"asc\"}]}"
