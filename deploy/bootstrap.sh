#!/usr/bin/env bash
# Idempotent stack bootstrap. Run by `make up` after Temporal reports healthy;
# safe to re-run at any time.
#
# Forgetting this step produces an empty customer list with no error anywhere,
# which is a confusing way to meet a new stack. It is wired into `make up`
# deliberately rather than documented as a manual step.
#
# Runs inside the temporal container, which ships the `temporal` CLI and can
# resolve the other services on the compose network.

set -euo pipefail

# The frontend binds the container's own IP, not 127.0.0.1.
TEMPORAL_ADDRESS="${TEMPORAL_ADDRESS:-$(hostname -i):7233}"
NAMESPACE="${TEMPORAL_NAMESPACE:-rewards}"
# 1h is Temporal's enforced minimum, not a preference -- see the create call below.
RETENTION="${TEMPORAL_RETENTION:-1h}"
ES_URL="${ES_URL:-http://elasticsearch:9200}"
ES_INDEX="${ES_INDEX:-temporal_visibility_v1_dev}"
ES_REFRESH_INTERVAL="${ES_REFRESH_INTERVAL:-200ms}"

log() { printf '  %s\n' "$*"; }

# Search attributes for the customer list page and the Temporal UI.
# Keep in sync with docs/PLAN.md section 4 and internal/rewards/searchattr.go.
declare -A SEARCH_ATTRS=(
  [CustomerId]=Keyword
  [CustomerEmail]=Keyword
  [CustomerName]=Text
  [RewardsLevel]=Keyword
  [RewardsPoints]=Int
  [RewardsEnrolledAt]=Datetime
  [RewardsGeneration]=Int
)

echo "==> Namespace '${NAMESPACE}' (retention ${RETENTION})"
if temporal operator namespace describe \
     --address "${TEMPORAL_ADDRESS}" --namespace "${NAMESPACE}" >/dev/null 2>&1; then
  log "already exists"
  # Retention is set at creation, so an existing namespace may disagree with
  # .env. Report rather than silently diverge.
  current="$(temporal operator namespace describe \
      --address "${TEMPORAL_ADDRESS}" --namespace "${NAMESPACE}" -o json 2>/dev/null \
      | grep -o '"workflowExecutionRetentionTtl"[^,}]*' || true)"
  [ -n "${current}" ] && log "retention now: ${current}"
else
  # Temporal enforces a 1h minimum retention and there is no way to lower it on
  # a released server, so anything shorter fails here with "A valid retention
  # period is not set on request". See docs/PLAN.md section 6.3; `make reap`
  # is how we force truncation instead.
  temporal operator namespace create \
    --address "${TEMPORAL_ADDRESS}" \
    --namespace "${NAMESPACE}" \
    --retention "${RETENTION}"
  log "created"
fi

# A freshly registered namespace is not immediately usable: the frontend serves
# namespaces from a cache that refreshes on an interval, so the very next call
# can still fail with "Namespace <name> is not found". Waiting here rather than
# letting a later step fail is the difference between a clean first run and a
# confusing one.
printf '  waiting for the namespace to become usable'
ready=0
for _ in $(seq 1 40); do
  if temporal operator search-attribute list \
       --address "${TEMPORAL_ADDRESS}" --namespace "${NAMESPACE}" >/dev/null 2>&1; then
    ready=1
    break
  fi
  printf '.'
  sleep 2
done
echo
if [ "${ready}" -ne 1 ]; then
  echo "ERROR: namespace ${NAMESPACE} still not resolvable after 80s." >&2
  exit 1
fi
log "ready"

echo "==> Search attributes"
# `search-attribute create` is idempotent server-side: re-registering an
# existing name and type succeeds and changes nothing, which is what makes this
# script safe to re-run. It does fail if the name exists with a *different*
# type, so a failure here is a real problem worth surfacing rather than
# swallowing.
for name in "${!SEARCH_ATTRS[@]}"; do
  type="${SEARCH_ATTRS[$name]}"
  if ! out="$(temporal operator search-attribute create \
                --address "${TEMPORAL_ADDRESS}" \
                --namespace "${NAMESPACE}" \
                --name "${name}" --type "${type}" 2>&1)"; then
    echo "ERROR: could not register search attribute ${name} (${type}):" >&2
    echo "${out}" >&2
    echo "If it already exists with a different type, it must be dropped first." >&2
    exit 1
  fi
  log "registered ${name} (${type})"
done

echo "==> Elasticsearch refresh interval (${ES_REFRESH_INTERVAL})"
# Temporal's visibility index template does not set refresh_interval, so the
# index inherits Elasticsearch's 1s default -- the larger half of the
# read-after-write lag. Registering a search attribute above is what creates
# the index, so this has to come after that.
if curl -sf "${ES_URL}/${ES_INDEX}" >/dev/null 2>&1; then
  curl -sf -XPUT "${ES_URL}/${ES_INDEX}/_settings" \
    -H 'Content-Type: application/json' \
    -d "{\"index\":{\"refresh_interval\":\"${ES_REFRESH_INTERVAL}\"}}" >/dev/null
  log "set on ${ES_INDEX}"
else
  log "index ${ES_INDEX} not found yet -- skipping (re-run 'make bootstrap' once it exists)"
fi

echo "==> Bootstrap complete"
