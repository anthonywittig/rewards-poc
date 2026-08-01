#!/usr/bin/env bash
# Force-delete closed Workflow Executions, immediately.
#
# Retention has a 1h floor, so this is how audit-log truncation
# (docs/FINDINGS.md#truncation-detection) gets demonstrated on demand.
#
#   make reap                      # every closed execution in the namespace
#   make reap WF=customer-abc123   # only that customer's closed runs
#
# Deletion is asynchronous; expect ~30s before a run stops resolving.

set -uo pipefail

ADDR="$(hostname -i):7233"
NS="${TEMPORAL_NAMESPACE:-rewards}"
WF="${WF:-}"

# The ExecutionStatus filter is load-bearing, not a nicety: `workflow delete`
# TERMINATES a running execution before deleting it. Without this clause a reap
# would destroy every active customer rather than just their old generations.
QUERY='ExecutionStatus != "Running"'
[ -n "${WF}" ] && QUERY="${QUERY} AND WorkflowId = \"${WF}\""

echo "Namespace: ${NS}"
echo "Query:     ${QUERY}"
echo

# Visibility is eventually consistent, so this listing and the batch job's own
# scan can disagree slightly at the margins.
temporal --address "${ADDR}" workflow list \
  --namespace "${NS}" --query "${QUERY}" --limit 25 2>/dev/null || true
echo

temporal --address "${ADDR}" workflow delete \
  --namespace "${NS}" --query "${QUERY}" \
  --reason "forced reap: demonstrating audit-log truncation" \
  --yes 2>&1 | tail -5

echo
echo "Batch delete requested. Propagation takes ~30s; re-check with 'make ps' or the UI."
