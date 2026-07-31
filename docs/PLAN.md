# Rewards POC — Phase 1 Plan

Demonstrating **Temporal as the system of record** for a customer rewards program.

The point of this POC is that there is no application database for rewards state. A
customer's points, tier, enrollment date, and full history of point-earning events live
entirely in a Temporal Workflow Execution and its Event History. We read current state via
Query, mutate it via Update, list/filter customers via Visibility search attributes, and
reconstruct the audit log by crawling raw Event History.

A second, equally weighted goal: **understand how Temporal actually uses Postgres and
Elasticsearch underneath.** Running both visibility backends and inspecting each is a
deliverable, not a side effect — see [§8](#8-inspecting-postgres-and-elasticsearch).

---

## 1. Decisions locked in

| Question | Decision |
|---|---|
| Stack | Go worker + Go HTTP API + React/TypeScript (Vite) UI |
| Visibility store | Elasticsearch, only. Postgres is persistence only — we care about it for how *workflows* are stored, not as a visibility backend |
| Audit log durability | Namespace retention set to **20 minutes**; continue-as-new input carries running totals; UI renders as much history as survives plus an explicit truncation notice |
| Deactivate | `CancelWorkflow` (graceful), not Terminate |
| Add points | Update (not Signal), so the UI gets synchronous success/failure |
| Continue-as-new | After **3** successful point-add updates (configurable) |
| Datastore inspection | An explicit POC goal — tooling and docs for looking inside both stores ([§8](#8-inspecting-postgres-and-elasticsearch)) |

The 20-minute retention is deliberate and is the most interesting choice here — see
[§6.3](#63-truncation-is-the-feature). It makes the history-truncation trade-off visible
within a coffee break instead of theoretical.

---

## 2. Architecture

```
┌───────────────┐     HTTP/JSON      ┌──────────────┐   gRPC    ┌──────────────────┐
│  web (React)  │ ─────────────────▶ │  api (Go)    │ ────────▶ │ Temporal Frontend│
│  Vite + TS    │                    │ Temporal     │           └────────┬─────────┘
└───────────────┘                    │ Client only  │                    │
                                     └──────────────┘        ┌───────────┴───────────┐
                                                             │                       │
                                                       ┌─────▼─────┐         ┌───────▼────────┐
┌───────────────┐        gRPC (poll)                   │ Postgres  │         │ Elasticsearch  │
│ worker (Go)   │ ◀────────────────────────────────────│ persistence│        │  visibility    │
│ workflow code │                                      └───────────┘         └────────────────┘
└───────────────┘
```

Five services in Compose plus the Temporal Web UI. `api` and `worker` are separate binaries
in one Go module so they share the workflow/state types — the API needs the same structs to
decode history payloads.

**The core demo has zero Activities.** Nothing external needs to be called; all state
transitions are pure functions of workflow state. That is exactly the "Temporal as a data
store" argument, and worth stating plainly in the README. We add one trivial Activity in
Phase 7 only to demonstrate the cancellation-cleanup path.

### Repo layout

```
cmd/worker/main.go            worker process
cmd/api/main.go               HTTP API process
internal/rewards/
  state.go                    CustomerState, tier thresholds, derived Level()
  workflow.go                 CustomerRewardsWorkflow
  workflow_test.go            unit tests via testsuite
  searchattr.go               typed search attribute keys
internal/history/
  audit.go                    multi-run history crawl → audit entries
internal/httpapi/             handlers, DTOs
web/                          Vite + React + TS
deploy/
  docker-compose.yml          full stack (Postgres + ES)
  dynamicconfig/dev.yaml      retention + visibility-lag overrides
  bootstrap.sh                namespace + search attribute registration
  inspect/                    canned psql + curl queries for §8
docs/
  PLAN.md                     this file
  DATASTORES.md               how Temporal uses Postgres and Elasticsearch
.env.example                  all ports + STACK_NAME
Makefile                      up / down / bootstrap / seed / test / psql / es
```

---

## 3. The workflow

One long-lived Entity Workflow per customer.

- **Workflow ID:** `customer-<customerID>` — gives us natural dedup and a stable handle for
  every subsequent operation. No lookup table needed.
- **Task queue:** `rewards`
- **Workflow Type:** `CustomerRewardsWorkflow`

### 3.1 State carried across continue-as-new

```go
type CustomerState struct {
    CustomerID string
    Name       string
    Email      string

    Points     int

    // Survives history reaping — this is what lets the audit log detect and
    // quantify its own truncation. See §6.3.
    EnrolledAt         time.Time // original enrollment, from the very first run
    LifetimeEarnEvents int       // count of all successful adds, ever
    LifetimePoints     int       // sum of all points ever added (≠ Points if we add spending later)
    Generation         int       // how many times we've continued-as-new
}
```

### 3.2 Tier is derived, never stored

```go
const (
    GoldThreshold     = 500
    PlatinumThreshold = 1000
)

func Level(points int) string {
    switch {
    case points >= PlatinumThreshold: return "platinum"
    case points >= GoldThreshold:     return "gold"
    default:                          return "basic"
    }
}
```

Deriving rather than storing means a threshold change applies uniformly and replays
identically. The trade-off — worth saying out loud to stakeholders — is that lowering a
threshold retroactively promotes everyone and raising it retroactively demotes them. A real
program would want an explicitly stored, monotonic "achieved tier."

### 3.3 Handlers

| Kind | Name | Signature |
|---|---|---|
| Update | `addPoints` | `AddPointsRequest{Amount int, Reason string} → AddPointsResult{Balance int, Level string, EventID string}` |
| Query | `getStatus` | `→ CustomerStatus{CustomerID, Name, Email, Points, Level, NextTierAt, EnrolledAt, LifetimeEarnEvents, Generation}` |
| Cancellation | — | graceful "leave the program" |

### 3.4 Validation — and a deliberate split

Requirement: points must be positive and must not exceed 1000 per transaction.

#### The mechanic

An Update runs in two phases, and **only the second one writes to Event History**:

| Outcome | Events written | In the audit log? |
|---|---|---|
| Validator returns an error | *none at all* | No |
| Validator passes, handler succeeds | `WorkflowExecutionUpdateAccepted` + `WorkflowExecutionUpdateCompleted` (success) | Yes |
| Validator passes, handler returns an error | `WorkflowExecutionUpdateAccepted` + `WorkflowExecutionUpdateCompleted` (failure) | Yes |

There is no `WorkflowExecutionUpdateRejected` event type — only those two exist. The docs are
blunt about what rejection means: the client is told it was rejected, and *"the Workflow will
have no indication that it was ever requested, similar to a Query handler."*

The caller cannot tell the two failure paths apart. Both come back as a failed Update with a
message, so the UI renders the same error either way. The only difference is whether a trace
survives.

The two events are also what make the audit log possible at all: Accepted carries the request
input and metadata from the invoker (our `Amount` and `Reason`), Completed carries the outcome
including whether it succeeded or failed. Pairing them by `AcceptedEventId` yields one audit
row holding both the ask and the answer — see [§6.2](#62-events-we-care-about).

#### The decision this forces

The stated requirement ("show the history of when points were added") is satisfied on the
happy path no matter where we validate, because successful adds are always recorded. What it
doesn't answer is whether a **rejected attempt** belongs in the audit log — and for a rewards
program the honest answer differs by reason:

- *"Amount was −50"* / *"amount was 6000"* — a client bug or a fat-finger. Nobody reviewing a
  customer's account cares. Recording it is noise.
- *"This add would push them past their lifetime cap"* — genuinely useful. A support rep
  asking "why didn't this customer reach platinum?" wants exactly that row.

So we use both phases for what each is good at:

- **Validator** (leaves no history) — `Amount <= 0`, `Amount > MaxPointsPerTxn`, empty reason.
- **Handler returning an error** (both events recorded) — one business rule: reject if the add
  would push `LifetimePoints` past a configured lifetime cap.

#### Why this is more than a stylistic choice

Two independent consequences push the same way.

**History bloat and abuse.** We continue-as-new every 3 adds, and history has hard limits
(50k events / 50 MB). Validator rejections are *free* — a buggy retry loop hammering
`amount: -1` cannot grow history at all. Handler rejections are recorded, so every rule
enforced there is a surface for history growth. Keep that set small and meaningful.

**Determinism surface.** Handler code is replayed; validator rejections leave nothing to
replay. If the lifetime cap lives in the handler and we later change the constant, we have
changed workflow code that must still reproduce already-recorded outcomes — an update recorded
as rejected under the old threshold could evaluate differently on replay. Rules in the
validator carry no such risk, because there is no recorded decision to contradict. This is a
real constraint on how freely the handler-side rules can be tuned later, and it is worth
verifying with the replay test in [§10](#10-testing).

Rule of thumb: **the validator is for facts about the request; the handler is for facts about
the customer.** Input shape, ranges, and required fields go in the validator. Anything that
depends on accumulated workflow state and that a human would want to see later goes in the
handler, accepting that it becomes part of the versioning surface.

#### Alternatives, if we change our minds

Both are defensible and cheap to switch to:

- **Everything in the validator** — simplest, cleanest history, audit log shows only
  successful adds. Right if we decide the audit log records what *happened*, not what was
  *attempted*.
- **Everything in the handler** — every attempt auditable, at the cost of history bloat and a
  larger determinism surface. Right if this were a financial ledger where attempted-and-denied
  is itself reportable.

The split is chosen partly because it lets the POC demonstrate both behaviours side by side:
trigger each rejection type and watch one appear in the Temporal UI's history while the other
leaves no trace. That is hard to internalize from documentation and easy to see once. The
README should walk through exactly that.

### 3.5 Continue-as-new after 3 updates

Two hard constraints from the platform:

1. **Continue-as-new is not supported inside an Update handler.** It must happen in the main
   workflow function.
2. **All handlers must be finished first**, or an in-flight Update is lost.

So the handler only mutates state and bumps a counter; the main function waits on it:

```go
func CustomerRewardsWorkflow(ctx workflow.Context, state CustomerState) error {
    earnsThisRun := 0

    // register getStatus query + addPoints update handlers here;
    // addPoints increments earnsThisRun on success

    if err := workflow.Await(ctx, func() bool {
        return earnsThisRun >= cfg.EarnsPerRun
    }); err != nil {
        // Cancelled — graceful departure. Cleanup, if any, needs a
        // disconnected context because ctx is already cancelled.
        return handleLeave(ctx, &state)
    }

    // Let any concurrently-accepted update finish before we roll the run.
    if err := workflow.Await(ctx, func() bool {
        return workflow.AllHandlersFinished(ctx)
    }); err != nil {
        return handleLeave(ctx, &state)
    }

    state.Generation++
    return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
}
```

`EarnsPerRun = 3` is artificially low to make the demo observable. Note in the README that
production code would instead use `workflow.GetInfo(ctx).GetContinueAsNewSuggested()`, which
lets the server decide based on actual history size. Make the constant configurable so both
behaviours are demonstrable.

### 3.6 Deactivation via cancel

`client.CancelWorkflow(ctx, "customer-<id>", "")`. The pending `workflow.Await` returns a
cancelled error, `handleLeave` records the departure and returns `ctx.Err()`, and the
execution closes as `Canceled`.

Cancel, not Terminate: Terminate skips workflow code entirely, so no cleanup runs and the
departure is never recorded by our own code.

Re-enrollment works because the workflow ID is free once the execution closes. Set
`WorkflowIDConflictPolicy: FAIL` on start so a double-create against a *running* customer
returns a clean 409 instead of silently attaching to the existing run.

**Re-enrolling starts over at zero points and basic tier** — decided, not a default. A
returning customer is simply a fresh enrollment that happens to reuse the workflow ID, so
there is no "restore the prior balance" path to build and no dependency on the old run's
history (which under a 20-minute retention is usually gone anyway). The UI should say so at
the point of deactivation, since it makes the action irreversible in a way "deactivate"
doesn't imply on its own.

---

## 4. Search attributes

Registered once at bootstrap, before any workflow starts. Using the typed API
(`temporal.NewSearchAttributeKey*` / `workflow.UpsertTypedSearchAttributes`).

| Name | Type | Written |
|---|---|---|
| `CustomerId` | Keyword | at start, re-asserted each run |
| `CustomerEmail` | Keyword | at start |
| `CustomerName` | Text | at start (tokenized → partial match) |
| `RewardsLevel` | Keyword | on every balance change |
| `RewardsPoints` | Int | on every balance change |
| `RewardsEnrolledAt` | Datetime | at start of each run, from carried state |
| `RewardsGeneration` | Int | on continue-as-new |

Built-ins we get free and will use: `ExecutionStatus` (Running vs Canceled → active vs
deactivated), `StartTime`, `CloseTime`, `WorkflowId`, `RunId`.

This powers the customer list page directly:
`RewardsLevel = "gold" AND ExecutionStatus = "Running" ORDER BY RewardsPoints DESC`.

Two notes:

- Search attributes should be inherited by the continued-as-new run, but re-upsert them at
  the top of every run anyway. It is cheap, and it means one code path establishes the
  invariant rather than relying on inheritance behaviour we would otherwise have to verify
  on every SDK upgrade.
- Registration must be idempotent — `bootstrap.sh` should tolerate "already exists" so
  `make up` is safe to re-run.

---

## 5. HTTP API

```
POST   /api/customers              → ExecuteWorkflow (conflict policy FAIL)
GET    /api/customers?q=<sql>      → ListWorkflow, projected from search attributes
GET    /api/customers/{id}         → QueryWorkflow(getStatus) + DescribeWorkflowExecution
POST   /api/customers/{id}/points  → UpdateWorkflow(addPoints), synchronous
DELETE /api/customers/{id}         → CancelWorkflow
GET    /api/customers/{id}/audit   → history crawl (§6)
```

The API holds a Temporal Client and nothing else — no database, no cache, no ORM. That is
the whole argument of the POC and the code should make it obvious at a glance.

Error mapping worth getting right, because these are the failure modes a reviewer will poke
at:

| Condition | HTTP |
|---|---|
| Update rejected by validator or handler | 422 + message |
| Customer already exists and is running | 409 |
| Workflow not found / history reaped | 404 |
| Query fails because no worker is polling | 503 + "worker unavailable" |
| Update lost to a continue-as-new race | retried once server-side, then 409 |

---

## 6. Audit log by crawling history

The interesting part, and the reason for the 3-update continue-as-new.

### 6.1 Walking the run chain

A customer's life spans many Runs sharing one Workflow ID. Newest run first, walk backwards:

1. `DescribeWorkflowExecution(workflowID, "")` → the current run.
2. `GetWorkflowHistory(workflowID, runID)` → events for that run.
3. The first event, `WorkflowExecutionStarted`, carries `ContinuedExecutionRunId`. Non-empty
   means there is a previous run; recurse. Empty means this is the original enrollment run
   and the chain is complete.
4. Reverse to get oldest-first.

### 6.2 Events we care about

| Event | Audit entry |
|---|---|
| `WorkflowExecutionStarted` (with empty `ContinuedExecutionRunId`) | Enrolled |
| `WorkflowExecutionUpdateAccepted` | the request: update name, `Amount`, `Reason`, update ID |
| `WorkflowExecutionUpdateCompleted` | the outcome: new balance and level, or failure message |
| `WorkflowExecutionContinuedAsNew` | generation boundary (render as a subtle divider) |
| `WorkflowExecutionCancelRequested` | Deactivated |

Accepted and Completed are separate events; pair them via `AcceptedEventId` on the completed
event to render one row with both the request and its result. Payloads are `Payloads` protos
— decode with the same `DataConverter` the client is configured with, which is why API and
worker share a Go module.

### 6.3 Truncation is the feature

With retention at 20 minutes and a new run every 3 point-adds, closed runs get reaped while
you watch. The audit log is designed around that rather than pretending it won't happen:

- Walking back, a non-empty `ContinuedExecutionRunId` whose `GetWorkflowHistory` returns
  `NotFound` means history was reaped. Stop and mark the result truncated.
- The carried `CustomerState` gives us ground truth to *quantify* the gap. If
  `LifetimeEarnEvents` is 23 and we could only reconstruct 7 rows, the UI says:

  > Showing 7 of 23 point events. Earlier history was deleted by Temporal's 20-minute
  > retention policy.

- `EnrolledAt` and the running totals survive in the continue-as-new payload, so the header
  of the detail page stays fully correct even when the log beneath it is partial.

This is the honest version of "Temporal as a data store," and demonstrating the limitation
is more valuable than hiding it.

### 6.4 Consequences of 20-minute retention to expect

- **Deactivated customers vanish from the list after ~20 minutes.** Their workflow is closed,
  so it gets reaped; the detail page starts returning 404. Active customers are never reaped
  (running executions are exempt), so this only affects deactivated ones.
- Temporal's default minimum retention for a *local* namespace is 1 hour, so 20 minutes
  requires a dynamic config override — `system.namespaceMinRetentionLocal` — before
  `namespace create --retention 20m` will be accepted.
- The reaping timer is jittered (`history.retentionTimerJitterDuration`), so deletion lands
  somewhere in `[retention, retention + jitter]`, not exactly at 20 minutes. Set the jitter
  low in dev config too. **Phase 0 must verify both keys empirically** — I confirmed
  `system.namespaceMinRetentionLocal` and its 1h default from the server source, but inferred
  the jitter key's exact name from the file's naming convention rather than reading it
  directly, so treat it as unverified until the stack is up.
- Cost: the crawl is O(runs × events) per page view, uncached. Fine at POC scale; note
  pagination and a cache as future work rather than building them now.

---

## 7. Local dev stack

Goal: `make up` on a laptop, no cloud, and several independent stacks side by side.

### 7.1 Services

`postgres` (persistence), `elasticsearch` (visibility), `temporal` (auto-setup image),
`temporal-ui`, `worker`, `api`, `web`.

Use `temporalio/auto-setup` with `DB=postgres12`, `ENABLE_ES=true`, `ES_SEEDS=elasticsearch`
— it creates schemas and installs the ES index template for us.

### 7.2 Running several stacks at once

Compose can't do arithmetic, so rather than a port-offset scheme, `.env` names every port
explicitly and `COMPOSE_PROJECT_NAME` isolates containers, networks, and named volumes:

```dotenv
STACK_NAME=alpha
COMPOSE_PROJECT_NAME=rewards-${STACK_NAME}

TEMPORAL_NAMESPACE=rewards
TEMPORAL_RETENTION=20m
EARNS_PER_RUN=3
MAX_POINTS_PER_TXN=1000

TEMPORAL_GRPC_PORT=7233
TEMPORAL_UI_PORT=8080
API_PORT=8081
WEB_PORT=5173
POSTGRES_PORT=5432
ES_PORT=9200

ES_JAVA_OPTS=-Xms256m -Xmx256m
```

A second stack is `cp .env.example .env.beta`, bump `STACK_NAME` and every port, then
`make up ENV=.env.beta`. Named volumes are project-prefixed automatically, so data isolation
is free.

Two things to call out in the README:

- **Elasticsearch is the memory hog**, and there is only one profile, so every stack pays for
  it. See [§7.3](#73-making-elasticsearch-cheap) for how far it can be pushed down.
- **A cheaper alternative to a whole second stack is a second namespace** on one stack. It
  gives isolated workflows and search attributes for a fraction of the RAM. Worth documenting
  as the default recommendation, with full stack duplication reserved for testing server
  config changes.

### 7.3 Making Elasticsearch cheap

We know this stack will hold tens of workflows, not millions, so ES can be tuned well below
its defaults. Starting point is Temporal's own reference Compose, which already sets a 256 MB
heap and — importantly — **absolute-byte disk watermarks** rather than percentages:

```yaml
elasticsearch:
  image: elasticsearch:7.17.27
  environment:
    - discovery.type=single-node
    - ES_JAVA_OPTS=-Xms256m -Xmx256m
    - xpack.security.enabled=false
    # Temporal's defaults — keep these verbatim
    - cluster.routing.allocation.disk.threshold_enabled=true
    - cluster.routing.allocation.disk.watermark.low=512mb
    - cluster.routing.allocation.disk.watermark.high=256mb
    - cluster.routing.allocation.disk.watermark.flood_stage=128mb
    # Our additions
    - xpack.ml.enabled=false
    - xpack.monitoring.collection.enabled=false
    - xpack.watcher.enabled=false
    - indices.memory.index_buffer_size=5%
  mem_limit: 768m
```

What each lever actually buys:

| Lever | Effect |
|---|---|
| `ES_JAVA_OPTS=-Xms256m -Xmx256m` | The dominant term. 256 MB is the practical floor; 128 MB sometimes boots but GC-thrashes, so treat it as an experiment, not a default. |
| `mem_limit: 768m` | Heap is only part of RSS — JVM overhead plus Lucene's off-heap structures add roughly 2×. Caps the blast radius so a misbehaving ES can't take the laptop with it. |
| `xpack.ml.enabled=false` | ML spawns native processes that allocate *outside* the heap. Pure waste here. Not in Temporal's default compose; worth adding. |
| `xpack.monitoring.collection.enabled=false`, `xpack.watcher.enabled=false` | Background indexing into system indices we will never read. |
| `indices.memory.index_buffer_size=5%` | Default is 10% of heap held for indexing buffers. Our write volume is a handful of docs per minute. |
| `discovery.type=single-node` | Skips cluster bootstrap and quorum machinery. |
Note that **shards and replicas need no attention** — Temporal's own visibility index
template already sets `number_of_shards: 1` and `number_of_replicas: 0` (plus
`auto_expand_replicas: "0-2"`, which stays at 0 on a single node). So the usual single-node
"cluster stuck at yellow" trap doesn't apply here; it's already handled upstream.

The absolute-byte watermarks deserve their own note in the README, because the default
*percentage* watermarks (85/90/95% of disk) are a nasty failure mode on a developer laptop: a
nearly-full disk flips ES indices to read-only, and then **visibility silently stops updating
while workflows keep running perfectly**. The customer list freezes, the detail pages stay
correct, and nothing logs an obvious error. Temporal's byte-based defaults avoid it; keep
them.

Realistically this lands ES around 500–700 MB RSS. Since ES is the only visibility store,
that is the floor per stack — if several stacks at once get tight, the lever is running
fewer stacks or using a second namespace on one stack (§7.2), not further ES tuning.

(OpenSearch is a supported drop-in alternative, but it is not meaningfully lighter, so it
isn't worth the substitution here.)

### 7.4 Bootstrap

`deploy/bootstrap.sh`, idempotent, run by `make up` after Temporal is healthy:

1. `temporal operator namespace create --retention ${TEMPORAL_RETENTION}` (tolerate exists)
2. `temporal operator search-attribute create` for each attribute in §4 (tolerate exists)
3. Lower the visibility index refresh interval (§7.5)
4. Wait for the frontend to report the namespace ready

Forgetting this step produces an empty customer list with no error, which is a confusing
first-run experience. It must be wired into `make up`, not documented as a manual step.

### 7.5 Cutting the visibility lag

Elasticsearch visibility is asynchronous, so a newly created or updated customer does not
appear in `ListWorkflow` results immediately. Out of the box that gap is roughly **1–2
seconds**, which is very visible in a UI. It has three components, and two of them are ours
to tune:

| Component | Default | Tunable? |
|---|---|---|
| Visibility task processing (history service drains `visibility_tasks`) | single-digit ms | Not worth touching |
| Bulk processor buffering before the write is sent to ES | **1s** (`worker.ESProcessorFlushInterval`) | Yes — dynamic config |
| ES index refresh, after which the document becomes searchable | **1s** (ES default for `index.refresh_interval`) | Yes — index setting |

The second one is a Temporal dynamic config setting; the default was raised from 200 ms to 1 s
in server v1.12 for throughput reasons that do not apply to us. In `dynamicconfig/dev.yaml`:

```yaml
worker.ESProcessorFlushInterval:
  - value: 100ms
```

The third is pure Elasticsearch, and it is the one people miss: **Temporal's visibility index
template does not set `refresh_interval` at all**, so it inherits the ES default of 1 second.
Set it explicitly in `bootstrap.sh` after the index exists:

```bash
curl -XPUT "$ES/temporal_visibility_v1_dev/_settings" \
  -H 'Content-Type: application/json' \
  -d '{"index":{"refresh_interval":"200ms"}}'
```

Together these bring the lag to roughly **200–300 ms** — below the threshold where a user
notices, and small enough that a single retry on the list page always wins.

**But not to true zero, and it can't be.** ES is a near-real-time search engine; the refresh
is *what makes* a document searchable, and Temporal's bulk processor exposes no
`refresh=wait_for` option to force it per write. Driving `refresh_interval` toward zero just
trades latency for constant segment churn.

So the honest answer has two halves. Tune the above, *and* don't route read-after-write
through visibility in the first place: `QueryWorkflow` and `DescribeWorkflowExecution` read
from persistence rather than the visibility store, so they are strongly consistent and have
no lag at all. That is why the create flow redirects to the detail page (§9) — it is not a
workaround for slow indexing, it is reading from the store that actually has the answer.
Visibility is only needed for the list page, which is inherently a "roughly now" view.

Worth knowing for [§8](#8-inspecting-postgres-and-elasticsearch): the lag is not a black box.
The `visibility_tasks` table in Postgres is the queue feeding this pipeline, so the delay can
be watched draining rather than taken on faith.

---

## 8. Inspecting Postgres and Elasticsearch

An explicit goal: understand *how Temporal actually uses* its two stores, not just that it
does. Postgres is interesting here for how it stores **workflows**; Elasticsearch is
interesting for how it stores the **searchable projection** of them. We are not evaluating
Postgres as a visibility backend — ES is the visibility store, full stop.

The framing is **persistence versus projection**: Postgres holds the truth about a workflow
in a form you cannot query, and Elasticsearch holds a queryable view of it that is neither
complete nor authoritative. Understanding why it is split that way is the goal.

Deliverable is `docs/DATASTORES.md` plus `make psql` / `make es` shortcuts and a handful of
canned queries in `deploy/inspect/`.

### 8.1 Postgres — how a workflow is actually stored

Temporal creates two databases. `temporal` is persistence and is the one we care about;
`temporal_visibility` gets its schema created by auto-setup but stays empty, because
Elasticsearch is our visibility store.

| Table | What it holds |
|---|---|
| `executions` | One row per run, holding serialized mutable state — our `CustomerState` lives here as an opaque blob |
| `current_executions` | Maps workflow ID → *current* run ID |
| `history_node` / `history_tree` | The Event History itself, as serialized batches of events |
| `buffered_events` | Events accepted but not yet committed to a workflow task |
| `visibility_tasks` | The async queue feeding Elasticsearch |
| `transfer_tasks`, `timer_tasks` | Internal scheduling queues |
| `task_queues`, `tasks` | The backlog the worker polls |
| `namespaces` | Namespace config, including our retention setting |

Four things worth demonstrating, because each one explains a decision elsewhere in this plan:

1. **`history_node` is opaque blobs.** You cannot `SELECT` a customer's point balance out of
   Postgres — the history is serialized event batches, not queryable rows. This is precisely
   *why* a separate visibility store has to exist at all, and why our audit log has to crawl
   history through the SDK rather than reading SQL. Show a `SELECT` returning binary next to
   the same data rendered by the audit crawl.
2. **`current_executions` is the continue-as-new indirection.** Watch `workflow_id` stay
   constant while `current_run_id` changes on every third point-add. That single row is what
   makes "the customer" a stable identity across many runs, and it is what the §6.1 crawl is
   walking backwards through.
3. **`visibility_tasks` drains asynchronously.** Add points and poll this table fast enough
   and you catch rows in flight. This *is* the read-after-write lag from
   [§7.5](#75-cutting-the-visibility-lag) — not an abstract caveat but a queue you can watch,
   and a direct way to confirm the flush-interval tuning actually did something.
4. **Retention deletion is visible at the storage layer.** After a continue-as-new, watch the
   closed run's rows disappear from `history_node` around the 20-minute mark. That is the
   audit-log truncation from [§6.3](#63-truncation-is-the-feature) happening in the database
   — the UI's "showing 7 of 23" message and these vanishing rows are the same event seen from
   two ends.

A canned query per finding in `deploy/inspect/`, each one small enough to read in full.

### 8.2 Elasticsearch — the visibility projection

One index, `temporal_visibility_v1_dev` by default. One document per Workflow Execution,
keyed by run ID.

The contrast with §8.1 is the point. Postgres stores *what happened*, losslessly and
unqueryably. ES stores *what is currently true and worth filtering on* — our seven search
attributes as real named fields (`RewardsLevel` is literally a keyword field called
`RewardsLevel`) plus the built-ins, and nothing else. No history, no state blob, no audit
trail. It is a lossy index built for one job.

Canned queries to ship:

```bash
# The index mapping — see our attributes as first-class fields
curl -s "$ES/temporal_visibility_v1_dev/_mapping?pretty"

# One customer's visibility document
curl -s "$ES/temporal_visibility_v1_dev/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"term":{"WorkflowId":"customer-abc"}}}'

# What the customer list page is really asking
curl -s "$ES/temporal_visibility_v1_dev/_search?pretty" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"bool":{"filter":[{"term":{"RewardsLevel":"gold"}},
      {"term":{"ExecutionStatus":"Running"}}]}},"sort":[{"RewardsPoints":"desc"}]}'

# Index size for our handful of customers — makes the ES-is-overkill point concretely
curl -s "$ES/_cat/indices/temporal_visibility*?v&h=index,docs.count,store.size"
```

Also worth capturing: the document for a **closed** (deactivated) customer, and then the same
query 20 minutes later after retention reaps it. ES does not decide to delete that document
on its own — it goes because the source in Postgres went. That is the projection relationship
made concrete.

### 8.3 Following one write all the way through

The artifact that ties it together: trace a single `addPoints` call end to end, with a query
at each hop.

```
UI click
  └─▶ Update accepted by the workflow
        └─▶ history_node          new event batch appended        (§8.1, query 1)
        └─▶ executions            mutable state blob rewritten
        └─▶ visibility_tasks      row enqueued                     (§8.1, query 3)
              └─▶ bulk processor  buffered up to flush interval    (§7.5)
                    └─▶ ES doc    upserted, searchable after refresh (§8.2)
```

Two conclusions fall out of it, and they are what `DATASTORES.md` should lead with:

- **The two stores answer different questions.** "What is this customer's balance and how did
  it get there?" is only answerable from persistence. "Which customers are gold and active?"
  is only answerable from ES. Neither can substitute for the other, which is why Temporal
  runs both rather than picking one.
- **ES is derived and disposable.** Every ES document can be rebuilt from persistence; nothing
  in persistence can be rebuilt from ES. That is why losing the ES index is an operational
  annoyance rather than data loss — and why the lag in §7.5 exists at all.

---

## 9. Web UI

Vite + React + TypeScript. Three screens:

- **Customer list** — table backed by `ListWorkflow`. Tier filter chips, a status toggle
  (Running/Canceled), sort by points, and a raw search-attribute query box so we can show the
  same query working in the Temporal UI. This is where search attributes earn their keep.
- **Create customer** — name + email, POSTs, redirects to detail.
- **Customer detail** — tier badge, points, progress bar to next tier, enrollment date;
  an add-points form showing synchronous success or the rejection message; a Deactivate
  button with confirmation; and the audit timeline with generation dividers and the
  truncation notice.

**The one UI gotcha that will bite:** Elasticsearch visibility is *asynchronous*, so after
creating a customer `ListWorkflow` will not include them for a beat. [§7.5](#75-cutting-the-visibility-lag)
tunes that down to ~200–300 ms, which is small enough to stop being a UX problem — but it is
never exactly zero, so the UI should not assume it is. Two rules follow:

- Create redirects straight to the **detail** page, which reads via Query and Describe. Those
  hit persistence rather than the visibility store, so they are strongly consistent regardless
  of how ES is behaving.
- The **list** page optimistically inserts the new row and re-fetches once shortly after,
  rather than trusting the first response to be complete.

Worth testing deliberately: create a customer and immediately hit the list endpoint. If the
tuning in §7.5 is working the row should be there within ~300 ms, and `visibility_tasks`
(§8.1) will show why when it isn't.

---

## 10. Testing

- **Unit** (`testsuite.WorkflowTestSuite`): tier boundaries at 499/500/999/1000; validator
  rejections; handler-side business rejection; continue-as-new fires after exactly 3 adds and
  carries state correctly; cancellation path. Note that the Go test environment needs
  `env.RegisterDelayedCallback` + `env.UpdateWorkflow` to drive updates.
- **Replay test** against a checked-in history JSON. Especially valuable here: entity
  workflows are long-lived, so they *will* outlive a deploy, and this is the single highest
  operational risk in the design.
- **Integration**: `temporal server start-dev` with `--search-attribute` flags, exercising
  the API end to end. Runs in CI without Docker or Elasticsearch — note that `start-dev` uses
  SQLite visibility, so it will not reproduce the ES lag of §7.5. That behaviour needs the
  real stack.

---

## 11. Phases

| # | Deliverable | Notes |
|---|---|---|
| 0 | Compose stack, dynamic config, bootstrap, Makefile, `.env.example` | Verify the 20m retention and jitter keys actually take effect — §6.4 |
| 1 | Workflow + worker: enroll, `addPoints` update, `getStatus` query, cancel, tier derivation, unit tests | Drive it entirely from `temporal` CLI before any UI exists |
| 2 | Continue-as-new after 3 adds, carrying totals | Includes the `AllHandlersFinished` guard |
| 3 | Go HTTP API + error mapping | |
| 4 | Search attributes end to end; list + filter | Demo the same query in both UIs |
| 5 | History crawl + truncation detection | |
| 6 | Datastore inspection: `DATASTORES.md`, `deploy/inspect/`, `make psql` / `make es`, the end-to-end write trace | Best done here — Phases 4–5 have generated data worth looking at, including reaped runs |
| 7 | React UI, all three screens | |
| 8 | Replay test, seed script, README, one cleanup Activity | |

Phases 1–2 are the substance and Phase 6 is the other headline deliverable; 0, 3–5, and 7 are
scaffolding around them.

---

## 12. Sharp edges

Things not in the original brief that will come up.

**Platform behaviour**

1. Continue-as-new inside an Update handler is unsupported — must live in the main function.
2. An Update in flight when a run rolls over gets aborted; the client must retry against the
   new run. With `EarnsPerRun = 3` this race is *frequent* — every third add can hit it. The
   API must retry transparently or the demo looks broken.
3. Update dedup via `UpdateID` is scoped to a single run, so it does **not** survive
   continue-as-new. A retry that straddles a rollover can double-apply. The UI should send a
   UUID per click, and we should document that per-run dedup is not sufficient for real money.
4. Queries fail with a confusing error when no worker is polling. Map it to a clear
   "worker unavailable" — during development the worker will be down often.
5. `workflow.Now()`, never `time.Now()`. Same for randomness and UUIDs.

**Operational**

6. **Versioning is the real risk.** Entity workflows run forever and will outlive deploys;
   changing workflow code under a running execution causes non-determinism errors. In dev,
   terminate stale workflows between changes. Document Worker Versioning / patching as the
   production answer, and add a `make reset` that wipes all customer workflows.
7. Customer names and emails land in Event History and are readable in plaintext in the
   Temporal UI. Fine for a POC; the production answer is a Codec Server. Say so explicitly,
   and use obviously fake seed data.
8. No authentication anywhere. State it in the README so nobody mistakes this for a starting
   point for something exposed.
9. Elasticsearch and Temporal server versions must be compatible (ES 7 needs Temporal 1.7+,
   ES 8 needs 1.18+). Pin both images.
10. ES visibility lag is tunable to ~200–300 ms but never to zero ([§7.5](#75-cutting-the-visibility-lag)).
    Anything needing read-after-write must go through Query or Describe, not `ListWorkflow`.

**Design**

11. Because Updates are serialized by the workflow, concurrent point-adds cannot lose an
    update — no optimistic locking, no transactions, no retry loop. This is a genuine
    advantage over the obvious Postgres implementation and deserves a callout in the README.
12. Points spending / expiry, tier downgrade over time, and tier-anniversary review are all
    out of scope, but the entity workflow with a durable timer is exactly where they'd go.
    Worth one paragraph as "what this shape buys you next."

---

## 13. Explicitly out of scope

Auth, multi-tenancy, points spending, tier expiry, archival, Codec Server, Worker Versioning,
production deployment, pagination and caching of the audit crawl, and horizontal scaling of
workers.
