#!/usr/bin/env bash
# §8.3: follow one addPoints call through Postgres and Elasticsearch.
# Invoked from the host by `make write-trace ID=inspect AMOUNT=10`.
#
# Expects compose project env already loaded by Make (COMPOSE, NAMESPACE, …)
# and a worker polling the rewards task queue.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

ENV_FILE="${ENV:-.env}"
ID="${ID:-inspect}"
AMOUNT="${AMOUNT:-10}"
REASON="${REASON:-write-trace}"
WF="customer-${ID}"

COMPOSE=(docker compose --env-file "${ENV_FILE}" -f deploy/docker-compose.yml)
PSQL=("${COMPOSE[@]}" exec -T postgres psql -U temporal -d temporal -v "ON_ERROR_STOP=1")
TCTL=("${COMPOSE[@]}" exec -T temporal)
TCLI=("${COMPOSE[@]}" exec -T \
  -e TEMPORAL_ADDRESS=temporal:7233 \
  -e "TEMPORAL_NAMESPACE=${NAMESPACE:?NAMESPACE must be set by Make}" \
  temporal temporal)

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

step "0. Before: current run pointer for ${WF}"
"${PSQL[@]}" -c "
SELECT workflow_id, encode(run_id,'hex') AS current_run_id_hex, state, status
FROM current_executions WHERE workflow_id = '${WF}';
"
before_nodes="$("${PSQL[@]}" -Atc "
SELECT count(*) FROM history_node h
JOIN executions e ON e.run_id = h.tree_id
WHERE e.workflow_id = '${WF}';
")"
echo "history_node rows for ${WF}: ${before_nodes}"

step "1. Poll visibility_tasks while an Update lands (catch the ES feed queue)"
(
  hit=0
  for i in $(seq 1 60); do
    c="$("${PSQL[@]}" -Atc 'SELECT count(*) FROM visibility_tasks;')"
    if [ "${c}" != "0" ]; then
      echo "HIT at poll ${i}: ${c} pending visibility task(s)"
      "${PSQL[@]}" -c "SELECT shard_id, task_id, length(data) AS blob_bytes, data_encoding FROM visibility_tasks;"
      hit=1
      break
    fi
    sleep 0.05
  done
  [ "${hit}" -eq 1 ] || echo "No in-flight row caught (drain was faster than the poller — re-run; this is normal)."
) &
poller=$!
sleep 0.05

step "2. UpdateWorkflow(addPoints) amount=${AMOUNT}"
"${TCLI[@]}" workflow update execute \
  --workflow-id "${WF}" \
  --name addPoints \
  --input "{\"amount\":${AMOUNT},\"reason\":\"${REASON}\"}"
wait "${poller}" || true

step "3. Postgres: history_node grew; executions mutable state rewritten"
after_nodes="$("${PSQL[@]}" -Atc "
SELECT count(*) FROM history_node h
JOIN executions e ON e.run_id = h.tree_id
WHERE e.workflow_id = '${WF}';
")"
echo "history_node rows for ${WF}: ${before_nodes} → ${after_nodes}"
"${PSQL[@]}" -c "
SELECT workflow_id,
       encode(run_id,'hex') AS run_id_hex,
       next_event_id,
       length(data) AS data_bytes,
       length(state) AS state_bytes
FROM executions
WHERE workflow_id = '${WF}'
ORDER BY next_event_id DESC
LIMIT 3;
"

step "4. visibility_tasks draining into the bulk processor"
drained=0
for i in $(seq 1 40); do
  c="$("${PSQL[@]}" -Atc 'SELECT count(*) FROM visibility_tasks;')"
  if [ "${c}" = "0" ]; then
    echo "pending_visibility_tasks: 0 (drained by poll ${i})"
    drained=1
    break
  fi
  sleep 0.05
done
if [ "${drained}" -ne 1 ]; then
  echo "still pending after ~2s (processor under load) — count=${c}"
  "${PSQL[@]}" -c 'SELECT count(*) AS pending_visibility_tasks FROM visibility_tasks;'
fi

step "5. Elasticsearch doc after refresh (RewardsPoints should reflect the add)"
# Small sleep covers the tuned refresh interval (~200ms) plus processor flush.
sleep 0.5
"${TCTL[@]}" curl -s "http://elasticsearch:9200/temporal_visibility_v1_dev/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d "{\"query\":{\"bool\":{\"filter\":[
        {\"term\":{\"WorkflowId\":\"${WF}\"}},
        {\"term\":{\"ExecutionStatus\":\"Running\"}}
      ]}},
      \"_source\":[\"WorkflowId\",\"RunId\",\"ExecutionStatus\",\"RewardsPoints\",\"RewardsLevel\",\"RewardsGeneration\",\"CustomerName\"]}"

step "Done"
cat <<EOF
Persistence answered "what happened to this customer?" (opaque history blobs).
ES answered "what is currently true and filterable?" (named fields, no history).
Neither store substitutes for the other — see docs/DATASTORES.md.
EOF
