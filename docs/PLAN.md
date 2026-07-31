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
| Visibility store | Postgres + Elasticsearch by default; a `lite` Compose profile drops ES and uses Postgres visibility |
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
  docker-compose.lite.yml     override: drop ES, Postgres visibility
  dynamicconfig/dev.yaml      retention overrides
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

- **Elasticsearch is the memory hog** — see [§7.3](#73-making-elasticsearch-cheap) for how far
  we can push it down. The `lite` profile (`docker-compose.lite.yml`) drops ES entirely and
  switches Temporal to Postgres visibility. Custom search attributes still work (Postgres 12+
  on Temporal 1.20+), so **the application code is identical** — only the Compose file
  differs. That is the profile for CI and for day-to-day work when you aren't specifically
  looking at ES.
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
| **`number_of_replicas: 0`** on the visibility index template | On a single node a replica can never be allocated, so the cluster sits at `yellow` forever and you waste an afternoon investigating a non-problem. Also drops per-shard bookkeeping. |
| `number_of_shards: 1` | Each shard carries fixed segment and translog overhead. Already the ES 7+ default — pin it so it stays true. |

The absolute-byte watermarks deserve their own note in the README, because the default
*percentage* watermarks (85/90/95% of disk) are a nasty failure mode on a developer laptop: a
nearly-full disk flips ES indices to read-only, and then **visibility silently stops updating
while workflows keep running perfectly**. The customer list freezes, the detail pages stay
correct, and nothing logs an obvious error. Temporal's byte-based defaults avoid it; keep
them.

Realistically this lands ES around 500–700 MB RSS. If that is still too much for running
several stacks at once, the honest answer is not more ES tuning — it is the `lite` profile,
with ES brought up only when you are actively demonstrating or inspecting it.

(OpenSearch is a supported drop-in alternative, but it is not meaningfully lighter, so it
isn't worth the substitution here.)

### 7.4 Bootstrap

`deploy/bootstrap.sh`, idempotent, run by `make up` after Temporal is healthy:

1. `temporal operator namespace create --retention ${TEMPORAL_RETENTION}` (tolerate exists)
2. `temporal operator search-attribute create` for each attribute in §4 (tolerate exists)
3. Wait for the frontend to report the namespace ready

Forgetting this step produces an empty customer list with no error, which is a confusing
first-run experience. It must be wired into `make up`, not documented as a manual step.

---

## 8. Inspecting Postgres and Elasticsearch

An explicit goal: understand *how Temporal actually uses* its two stores, not just that it
does. This is where running both visibility backends pays off, because the same seven search
attributes are stored in dramatically different ways.

Deliverable is `docs/DATASTORES.md` plus `make psql` / `make es` shortcuts and a handful of
canned queries in `deploy/inspect/`.

### 8.1 Postgres — persistence

Temporal creates two databases: `temporal` (persistence, always used) and
`temporal_visibility` (only used when ES is off). The tables worth looking at in `temporal`:

| Table | What it shows |
|---|---|
| `executions` | One row per execution, holding serialized mutable state — our `CustomerState` lives in here as an opaque blob |
| `current_executions` | Maps workflow ID → current run ID. This is the indirection that makes continue-as-new work |
| `history_node` / `history_tree` | The actual Event History, stored as **serialized batches of events** |
| `visibility_tasks` | The async queue that feeds Elasticsearch |
| `task_queues`, `tasks` | Task queue backlog the worker polls |
| `timer_tasks`, `transfer_tasks` | Internal scheduling queues |
| `namespaces` | Namespace config — including the search-attribute name→column mapping |

Two findings to demonstrate explicitly, because they justify the whole architecture:

1. **`history_node` is opaque blobs.** You cannot `SELECT` a customer's point balance out of
   Postgres. The event history is not queryable data — which is precisely *why* a separate
   visibility store has to exist. Show a `SELECT` against it returning binary, next to the
   same information rendered by our audit-log crawl.
2. **`visibility_tasks` drains asynchronously.** Add points and query this table fast enough
   and you will catch rows in flight. This *is* the read-after-write lag from
   [§9](#9-web-ui) — not an abstract caveat but a queue you can watch. A canned query that
   polls its row count while you add points makes the point better than any paragraph.

### 8.2 Postgres — visibility (the `lite` profile)

`executions_visibility` has a `search_attributes` JSONB column, plus **pre-allocated,
generically-named typed columns** populated as `GENERATED ALWAYS AS ... STORED` projections
out of that JSON:

`Bool01–03`, `Double01–03`, `Int01–03`, `Datetime01–03`, `Text01–03` (TSVECTOR),
`Keyword01–10`, `KeywordList01–03` — plus a parallel `Temporal`-prefixed set for built-ins.

So our `RewardsLevel` does not exist as a column called `RewardsLevel`. It gets *assigned* to
something like `Keyword01`, and the logical→physical mapping is held in the namespace config.
That explains two things the docs mention only in passing:

- Custom search attributes on SQL must be registered per-namespace (the mapping is namespace
  scoped).
- Deleting a search attribute frees the mapping **but not the data**, so reusing a name can
  surface a previous attribute's values. Worth actually reproducing — create, delete, and
  recreate an attribute and watch stale data appear.

It also explains the hard ceiling: ten Keyword attributes, three Ints, three Datetimes. We
use one Int (`RewardsPoints`), one more for `RewardsGeneration`, one Datetime, one Text, and
two Keywords — comfortably inside the budget, but a real program with dozens of attributes
would hit the wall. Good thing to state, since it is a genuine reason to reach for ES.

### 8.3 Elasticsearch — visibility (default profile)

One index, `temporal_visibility_v1_dev` by default. One document per Workflow Execution,
keyed by run ID.

Contrast with §8.2, which is the headline finding: in ES the custom search attributes are
**real, named fields**. `RewardsLevel` is a keyword field literally called `RewardsLevel`. No
mapping table, no numbered-column budget, and adding a new attribute is a mapping update
rather than a scarce-slot allocation.

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
query 20 minutes later after retention reaps it — showing that ES is a *projection* of
persistence, not an independent record, and that it disappears along with the source.

### 8.4 The side-by-side

The payoff is running the identical `RewardsLevel = "gold"` list-filter against both profiles
and showing the same result served by a `Keyword01` column in one and a named ES field in the
other, with the application code untouched. A short table in `DATASTORES.md` comparing
storage shape, attribute limits, write consistency (synchronous vs asynchronous), and
resource cost is probably the single most useful artifact this POC produces for anyone
choosing a visibility backend.

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

**The one UI gotcha that will bite:** Elasticsearch visibility is *asynchronous*. After
creating a customer, `ListWorkflow` will not include them for a beat. Redirect straight to
the detail page — which reads via Query and Describe, both strongly consistent — and on the
list page, poll briefly or optimistically insert. Under the `lite` (Postgres) profile writes
are synchronous, so this bug is invisible there and will only appear in the default stack.

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
  the API end to end. Runs in CI without Docker.

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
| 6 | Datastore inspection: `DATASTORES.md`, `deploy/inspect/`, `make psql` / `make es`, both-profile side-by-side | Best done here — Phases 4–5 have generated data worth looking at |
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

**Design**

10. Because Updates are serialized by the workflow, concurrent point-adds cannot lose an
    update — no optimistic locking, no transactions, no retry loop. This is a genuine
    advantage over the obvious Postgres implementation and deserves a callout in the README.
11. Points spending / expiry, tier downgrade over time, and tier-anniversary review are all
    out of scope, but the entity workflow with a durable timer is exactly where they'd go.
    Worth one paragraph as "what this shape buys you next."

---

## 13. Explicitly out of scope

Auth, multi-tenancy, points spending, tier expiry, archival, Codec Server, Worker Versioning,
production deployment, pagination and caching of the audit crawl, and horizontal scaling of
workers.
