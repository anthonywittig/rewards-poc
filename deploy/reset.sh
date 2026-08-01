#!/usr/bin/env bash
# Wipe every customer workflow in the namespace, running ones included.
#
# A development affordance, and a deliberately blunt one. Entity workflows
# outlive deploys (docs/PLAN.md section 12.11), so while iterating on workflow
# code you will routinely have executions recorded by yesterday's version that
# today's version cannot replay. Production answers that with versioning; in dev
# the honest answer is usually to throw the customers away and start again.
#
#   make reset
#
# This is NOT what `make reap` does. reap deletes *closed* executions to force
# audit-log truncation and deliberately spares running ones. reset deletes
# everything -- `workflow delete` terminates a running execution first -- so the
# distinction between them is exactly the ExecutionStatus filter reap carries.
#
# Deletion is asynchronous; expect ~30s before executions stop resolving.

set -uo pipefail

ADDR="$(hostname -i):7233"
NS="${TEMPORAL_NAMESPACE:-rewards}"

# Scoped to our workflow type rather than the whole namespace, so a shared dev
# namespace does not lose someone else's work to a `make reset` typed here.
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
echo "Repopulate with: make seed"
