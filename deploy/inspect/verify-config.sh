#!/usr/bin/env bash
# Pins the platform behaviour the plan depends on. docs/PLAN.md section 6.4
# flagged these as needing empirical verification before anything is built on
# top of them; this is that verification, kept as a script so it can be re-run
# after a server upgrade.
#
# Run inside the temporal container: make verify-config
#
# Checks:
#   1. Minimum namespace retention. The plan wanted 20m. Temporal enforces a
#      1h floor and the key that would lower it (system.namespaceMinRetentionLocal)
#      is not registered in released servers. This asserts the floor is still 1h
#      so we find out immediately if a future version relaxes it.
#   2. Every key in dynamicconfig/dev.yaml is actually registered. An unregistered
#      key is loaded and then silently ignored -- the server warns once at startup
#      and otherwise behaves as if you had not set it.
#   3. On-demand deletion works, which is how truncation gets demonstrated now
#      that sub-hour retention is unavailable.

set -uo pipefail

ADDR="$(hostname -i):7233"
t() { temporal --address "${ADDR}" "$@"; }
fails=0
pass() { printf '  PASS  %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

echo "=== 1. Minimum namespace retention ==="
probe_retention() {
  local ns="verify-ret-$$-${2}"
  if t operator namespace create --namespace "${ns}" --retention "$1" >/dev/null 2>&1; then
    t operator namespace delete --namespace "${ns}" --yes >/dev/null 2>&1 || true
    return 0
  fi
  return 1
}

if probe_retention 59m a; then
  fail "59m was accepted -- the 1h floor has been relaxed. Revisit docs/PLAN.md 6.3;"
  echo "        a shorter retention would let us drop the 'make reap' workaround."
else
  pass "59m rejected (1h floor still enforced, as expected)"
fi

if probe_retention 1h b; then
  pass "1h accepted"
else
  fail "1h rejected -- the floor moved up. TEMPORAL_RETENTION in .env needs raising."
fi

echo
echo "=== 2. Dynamic config keys are registered ==="
# The server logs `unregistered key "<name>"` at startup for anything it does
# not recognise, then ignores it. Checking the log is the only reliable signal;
# there is no API that reports whether a key took effect.
#
# The key table lives in the server binary. Two things to get right: `strings`
# runs adjacent constants together into long blobs, so match as a substring
# rather than a whole line; and the output is ~25MB, so pipe it straight into
# grep instead of capturing it in a shell variable.
SERVER_BIN=/usr/local/bin/temporal-server
#
# Use `grep -c`, not `grep -q`: -q exits on the first match, `strings` then dies
# of SIGPIPE, and `set -o pipefail` reports the whole pipeline as failed even
# though the key was found. -c consumes the stream to the end.
for key in history.retentionTimerJitterDuration worker.ESProcessorFlushInterval; do
  hits="$(strings "${SERVER_BIN}" 2>/dev/null | grep -cF "${key}" || true)"
  if [ "${hits:-0}" -gt 0 ]; then
    pass "${key} is a registered key"
  else
    fail "${key} is NOT registered by this server -- it will be silently ignored"
    echo "        after one 'unregistered key' warning at startup. Find the current"
    echo "        name with: strings ${SERVER_BIN} | grep -oE \\"
    echo "          '\\b(system|frontend|history|worker|matching)\\.[A-Za-z]+'"
  fi
done

echo
echo "=== 3. On-demand deletion (how 'make reap' works) ==="
NS="verify-del-$$"
if ! t operator namespace create --namespace "${NS}" --retention 1h >/dev/null 2>&1; then
  fail "could not create scratch namespace, skipping"
else
  for _ in $(seq 1 30); do
    t workflow list --namespace "${NS}" --limit 1 >/dev/null 2>&1 && break
    sleep 2
  done
  # No worker polls this task queue, so the workflow stays Running until we
  # terminate it -- and terminated is a closed state, which is all we need.
  t workflow start --namespace "${NS}" --workflow-id del-probe \
    --type VerifyWorkflow --task-queue no-worker-listens-here >/dev/null 2>&1
  t workflow terminate --namespace "${NS}" --workflow-id del-probe --reason verify >/dev/null 2>&1
  t workflow delete --namespace "${NS}" --workflow-id del-probe >/dev/null 2>&1

  deleted=0
  for i in $(seq 1 18); do
    if ! t workflow describe --namespace "${NS}" --workflow-id del-probe >/dev/null 2>&1; then
      pass "closed execution deleted on demand (~$((i * 5))s -- deletion is async)"
      deleted=1
      break
    fi
    sleep 5
  done
  [ "${deleted}" -eq 1 ] || fail "execution survived 90s after delete"
  t operator namespace delete --namespace "${NS}" --yes >/dev/null 2>&1 || true
fi

echo
if [ "${fails}" -eq 0 ]; then
  echo "All checks passed."
else
  echo "${fails} check(s) failed."
fi
exit "${fails}"
