#!/usr/bin/env bash
# Wipe every customer workflow in the namespace, running ones included.
#
# A dev affordance for when executions recorded by yesterday's workflow code
# cannot be replayed by today's:
#
#   make reset
#
# Deletion is asynchronous; expect ~30s before executions stop resolving.

set -uo pipefail

ADDR="$(hostname -i):7233"
NS="${TEMPORAL_NAMESPACE:-rewards}"

# Scoped to our workflow type, so a shared dev namespace does not lose someone
# else's work.
QUERY='WorkflowType = "CustomerRewardsWorkflow"'

echo "Namespace: ${NS}"
echo "Query:     ${QUERY}"
echo "This deletes EVERY customer, running or not."
echo

temporal --address "${ADDR}" workflow count \
  --namespace "${NS}" --query "${QUERY}" 2>/dev/null || true
echo

temporal --address "${ADDR}" workflow delete \
  --namespace "${NS}" --query "${QUERY}" \
  --reason "make reset: clearing customer workflows for a fresh start" \
  --yes 2>&1 | tail -5

echo
echo "Delete requested. Propagation takes ~30s."
