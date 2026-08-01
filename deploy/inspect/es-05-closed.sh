#!/usr/bin/env bash
# §8.2: a closed (deactivated) customer's visibility document, and the same
# query after make reap. ES does not decide to delete on its own — the doc goes
# because the source execution in Postgres went. That is the projection
# relationship made concrete.
#
# Usage:
#   make deactivate ID=leave
#   make inspect-es Q=closed ID=leave          # expect ExecutionStatus=Completed
#   make reap WF=customer-leave
#   # wait ~30s
#   make inspect-es Q=closed ID=leave          # expect hits.total=0

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"
INDEX="${INDEX:-temporal_visibility_v1_dev}"
WF="${WF:-customer-leave}"

echo "=== visibility docs for WorkflowId=${WF} (closed / after reap) ==="
echo
curl -s "${ES}/${INDEX}/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d "{\"query\":{\"term\":{\"WorkflowId\":\"${WF}\"}},\"size\":10,\"_source\":[\"WorkflowId\",\"RunId\",\"ExecutionStatus\",\"CloseTime\",\"RewardsPoints\",\"RewardsLevel\"]}"
