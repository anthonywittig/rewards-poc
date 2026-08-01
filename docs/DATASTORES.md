# How Temporal uses Postgres and Elasticsearch

This POC stores all rewards state in Temporal Workflow Executions. There is no
application database. Temporal itself still has two stores underneath, and they
answer **different questions**:

| Store | Role | Answers |
|---|---|---|
| **Postgres** (`temporal`) | Persistence — source of truth | What happened to this workflow? (event history + mutable state, as opaque blobs) |
| **Elasticsearch** (`temporal_visibility_v1_dev`) | Visibility — searchable projection | Which workflows match a filter right now? (named fields, no history) |

Neither can substitute for the other. That split is why Temporal runs both, why
our audit log crawls history through the SDK rather than SQL, and why the
customer list goes through `ListWorkflow` / ES rather than `Query`.

Two conclusions to keep:

1. **The two stores answer different questions.** "What is this customer's
   balance and how did it get there?" is only answerable from persistence.
   "Which customers are gold and active?" is only answerable from ES.
2. **ES is derived and disposable.** Every ES document can be rebuilt from
   persistence; nothing in persistence can be rebuilt from ES. Losing the index
   is an operational annoyance, not data loss — and that is also why visibility
   lag exists at all ([FINDINGS.md](FINDINGS.md#visibility-lag)).

The rest of this doc is how to *see* that for yourself against the local stack.
Everything below was measured against Temporal server **1.29.7** with
Elasticsearch **7.17.27** and `ENABLE_ES=true`; what it established is recorded
in [FINDINGS.md](FINDINGS.md).

---

## Prerequisites

```sh
make up          # Postgres + ES + Temporal + UI + worker + api, seeded
make enroll ID=inspect NAME="Inspect Ada" EMAIL=inspect@example.com
# a few adds so continue-as-new has something to show:
for i in 1 2 3 4 5 6 7; do make add ID=inspect AMOUNT=100 REASON="add $i"; done
```

Canned queries:

```sh
make inspect                 # catalog
make inspect-pg Q=history-blob ID=inspect
make inspect-es Q=customer ID=inspect
make write-trace ID=inspect AMOUNT=10
```

`make psql` / `make es` without `Q=` remain interactive / summary shortcuts.
With `Q=` they delegate to the same canned queries (`make psql Q=history-blob`).

Scripts under `deploy/inspect/` are intentionally small enough to read in full.
The temporal container has `bash`, `curl`, and `grep` but **no `jq` / `python3`**,
so ES helpers are plain `curl`.

---

## Postgres — how a workflow is actually stored

With `ENABLE_ES=true`, auto-setup creates **one** database: `temporal`. There is
no `temporal_visibility` database on this stack — visibility lives entirely in
Elasticsearch. (An earlier plan draft assumed a second, empty Postgres database;
that is what you get with *Postgres* visibility, not with ES.)

Useful tables for this POC:

| Table | What it holds |
|---|---|
| `executions` | One row per **run**, holding serialized mutable state (`data` / `state`, Proto3) |
| `current_executions` | Maps `workflow_id` → *current* `run_id` |
| `history_node` / `history_tree` | Event History as serialized batches (`data` bytea) |
| `visibility_tasks` | Async queue feeding Elasticsearch |
| `transfer_tasks`, `timer_tasks` | Internal scheduling queues |
| `task_queues`, `tasks` | Backlog the worker polls |
| `namespaces` | Namespace config (retention is easier to read via `temporal operator namespace describe`) |

`run_id` is `bytea`. `encode(run_id, 'hex')` yields the UUID without dashes;
Elasticsearch stores the same UUID *with* dashes.

### 1. `history_node` is opaque blobs

```sh
make inspect-pg Q=history-blob ID=inspect
```

You get `blob_bytes`, `data_encoding = Proto3`, and a hex prefix — not points,
not tier, not a reason string. The customer's balance is not a SQL column. That
is exactly why a separate visibility store has to exist, and why the audit log
([FINDINGS.md](FINDINGS.md#the-history-crawl)) walks history through the SDK's
`GetWorkflowHistory` rather than querying Postgres.

Join hint used by the canned query: `history_node.tree_id = executions.run_id`.

### 2. `current_executions` is the continue-as-new indirection

```sh
make inspect-pg Q=current-run ID=inspect
```

After seven point-adds (`EarnsPerRun = 3`), `workflow_id` is still
`customer-inspect` while `current_run_id` has moved. `executions` still holds a
row per retained generation. That single `current_executions` row is what makes
"the customer" a stable identity across many runs — and what the audit crawl
walks backwards through via `ContinuedExecutionRunId`.

### 3. `visibility_tasks` drains asynchronously

```sh
make inspect-pg Q=visibility-tasks
# usually empty at rest
```

At rest the table is empty. Poll it in a tight loop while another terminal runs
`make add`, or use `make write-trace`, and you catch a Proto3 row in flight for
a few tens of milliseconds. **That row is the read-after-write lag** from
[FINDINGS.md](FINDINGS.md#visibility-lag) — not an abstract caveat, a queue you
can watch. Confirming `worker.ESProcessorFlushInterval` did something is the
same observation.

### 4. Retention / `make reap` is visible at the storage layer

```sh
make inspect-pg Q=after-reap ID=inspect   # several executions rows if CAN happened
make reap WF=customer-inspect             # deletes ContinuedAsNew gens only
# wait ~30s
make inspect-pg Q=after-reap ID=inspect   # one row left while still Running
```

Closed runs lose their `executions` and `history_node` rows once delete
propagates (~25–40s; deletion is async). The Running run is untouched because
`reap.sh` filters `ExecutionStatus != "Running"`. The UI's "showing 7 of 23"
truncation notice and these vanishing rows are the same event from two ends
([FINDINGS.md](FINDINGS.md#truncation-detection)).

---

## Elasticsearch — the searchable projection

One index, `temporal_visibility_v1_dev`. **One document per Workflow Execution
(run)**, keyed roughly `WorkflowId~RunId`.

Postgres stores *what happened*, losslessly and unqueryably. ES stores *what is
currently true and worth filtering on* — our custom search attributes as real
named fields (`RewardsLevel` is literally a keyword field called
`RewardsLevel`) plus built-ins (`ExecutionStatus`, `StartTime`, …). No history,
no state blob, no audit trail.

```sh
make inspect-es Q=mapping
make inspect-es Q=customer ID=inspect
make inspect-es Q=gold-running
make inspect-es Q=indices
```

A customer who continued-as-new twice shows **three** hits for the same
`WorkflowId` (two `ContinuedAsNew` + one `Running`) until `make reap` removes
the closed ones. List UIs that want "one row per customer" must exclude
rolled-over generations with `ExecutionStatus != "ContinuedAsNew"` — *not*
`ExecutionStatus = "Running"`, which silently drops enrollments that failed
validation and any ops-closed run
([FINDINGS.md](FINDINGS.md#visibility-indexes-runs-not-customers)).

### Sorting: ES can, Temporal ListWorkflow cannot

`make inspect-es Q=gold-running` sorts by `RewardsPoints` descending and it
works. The equivalent visibility query does not:

```text
temporal workflow list --query 'RewardsLevel = "gold" ORDER BY RewardsPoints DESC'
→ ORDER BY clause is not supported
```

So the limitation in [FINDINGS.md](FINDINGS.md#order-by-is-not-supported) is in
Temporal's visibility query language / frontend, **not** in Elasticsearch. For
this POC, fetch the filtered set and sort in the API.

### Closed generations, then reap

```sh
make inspect-es Q=closed ID=inspect    # ContinuedAsNew docs + the Running one
make reap WF=customer-inspect
# wait ~30s
make inspect-es Q=closed ID=inspect    # only the Running doc survives
```

ES does not decide to delete those documents on its own — they go because the
source executions in Postgres went. Projection relationship, made concrete.

Note this demo uses **rolled-over generations**, not a departed customer.
Deactivation is soft ([FINDINGS.md](FINDINGS.md#soft-deactivation)): the
execution stays `Running` with `RewardsActive: false`, so it is neither closed
nor reapable. Only an ops-level cancel or terminate produces a closed run for a
customer who left.

**Caveat:** `_cat/indices` `docs.count` can stay inflated after deletes until
Lucene merges drop soft-deleted docs (`docs.deleted` in the canned indices
query). Prefer `_search` / `_count` for "is it gone?"

Index size for a handful of customers is tens of KB. That is the
"Elasticsearch is overkill for this POC" point with numbers.

---

## Following one write all the way through

```sh
make write-trace ID=inspect AMOUNT=10
```

```
make add / UpdateWorkflow(addPoints)
  └─▶ history_node          new event batch appended        (inspect-pg Q=history-blob)
  └─▶ executions            mutable state blob rewritten
  └─▶ visibility_tasks      row enqueued briefly            (inspect-pg Q=visibility-tasks)
        └─▶ bulk processor  buffered up to flush interval   (FINDINGS: visibility lag)
              └─▶ ES doc    upserted, searchable after refresh (inspect-es Q=customer)
```

`write-trace` polls `visibility_tasks` while the Update runs, then shows the
Postgres row growth and the Running ES document's `RewardsPoints`. If the
poller misses the in-flight row, drain was simply faster than the loop — re-run;
catching it is probabilistic at our flush settings, which is itself the lesson.

---

## Quick reference

| Question | Where to look |
|---|---|
| Current balance / tier (authoritative) | Workflow Query (`make status` / API) |
| Event-by-event audit | History crawl via SDK, not SQL |
| Filter gold + running | ES / `ListWorkflow` |
| Sort by points | Client-side (or raw ES); not `ListWorkflow ORDER BY` |
| Why list lags after create | `visibility_tasks` + ES refresh interval |
| Why audit log truncates | Closed runs deleted from `executions` / `history_node` |

---

## Schema notes for further SQL

Useful if you write queries beyond the canned ones. All observed on 1.29.7.

- `history_node.tree_id` equals `executions.run_id` for these entity workflows —
  that is the join the canned SQL uses.
- `data_encoding` is `Proto3` for `history_node.data`, `executions.data` and
  `executions.state`. There is no text representation to fall back on.
- `run_id` is `bytea` in Postgres. `encode(run_id, 'hex')` yields the UUID
  without dashes; Elasticsearch stores the same UUID *with* dashes, so
  cross-store comparisons need one or the other normalised.
- `visibility_tasks` is empty at rest, so a `SELECT count(*)` after a write
  reports 0 even though the queue was used. Poll alongside the write.

What these inspections established about Temporal's behaviour more broadly —
runs-vs-customers in visibility, `ORDER BY`, the read-after-write lag — is
recorded in [FINDINGS.md](FINDINGS.md).
