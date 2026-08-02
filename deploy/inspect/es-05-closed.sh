#!/usr/bin/env bash
# One customer's visibility documents either side of make reap. ES does not
# decide to delete on its own — the docs go because the source executions in
# Postgres went. That is the projection relationship made concrete.
#
# Note this shows *rolled-over generations* being reaped, not a departed
# customer. Deactivation is soft: the execution stays Running with
# RewardsActive=false, so it is neither closed nor reapable. Only an ops-level
# cancel or terminate closes a customer's run.
#
# Usage (after a few adds, so continue-as-new has happened):
#   make inspect-es Q=closed ID=inspect   # ContinuedAsNew docs + the Running one
#   make reap WF=customer-inspect
#   # wait ~30s
#   make inspect-es Q=closed ID=inspect   # only the Running doc survives

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"
INDEX="${INDEX:-temporal_visibility_v1_dev}"
WF="${WF:-customer-inspect}"

echo "=== visibility docs for WorkflowId=${WF} (all generations / after reap) ==="
echo
curl -s "${ES}/${INDEX}/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d "{\"query\":{\"term\":{\"WorkflowId\":\"${WF}\"}},\"size\":10,\"_source\":[\"WorkflowId\",\"RunId\",\"ExecutionStatus\",\"CloseTime\",\"RewardsPoints\",\"RewardsLevel\",\"RewardsActive\"]}"
