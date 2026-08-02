#!/usr/bin/env bash
# what the customer-list page is really asking.
#
# Note the sort on RewardsPoints: raw Elasticsearch happily accepts it.
# Temporal's ListWorkflow visibility query language rejects ORDER BY entirely
# (custom *and* built-in attributes) — confirmed on server 1.29.7. Sorting for
# the UI therefore has to happen client-side.
#
# Run via: make inspect-es Q=gold-running

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"
INDEX="${INDEX:-temporal_visibility_v1_dev}"

echo "=== RewardsLevel=gold AND ExecutionStatus=Running, sorted by RewardsPoints ==="
echo "(this sort works in ES; the equivalent ListWorkflow query with ORDER BY fails)"
echo
curl -s "${ES}/${INDEX}/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"bool":{"filter":[
        {"term":{"RewardsLevel":"gold"}},
        {"term":{"ExecutionStatus":"Running"}}
      ]}},
      "sort":[{"RewardsPoints":"desc"}],
      "_source":["WorkflowId","CustomerName","RewardsLevel","RewardsPoints","ExecutionStatus","RewardsGeneration"]}'
