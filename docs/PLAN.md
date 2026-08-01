# Rewards POC — Phase 1 Plan

Demonstrating **Temporal as the system of record** for a customer rewards program.

The point of this POC is that there is no application database for rewards state. A
customer's points, tier, enrollment date, and full history of point-earning events live
entirely in a Temporal Workflow Execution and its Event History. We read current state via
Query, mutate it via Update, list/filter customers via Visibility search attributes, and
reconstruct the audit log by crawling raw Event History.

A second, equally weighted goal: **understand how Temporal actually uses Postgres and
Elasticsearch underneath** — Postgres for how it stores workflows, Elasticsearch for the
searchable projection it builds from them. Inspecting both is a deliverable, not a side
effect — see [§8](#8-inspecting-postgres-and-elasticsearch).

---

## 1. Decisions locked in

| Question | Decision |
|---|---|
| Stack | Go worker + Go HTTP API + React/TypeScript (Vite) UI |
| Visibility store | Elasticsearch, only. Postgres is persistence only — we care about it for how *workflows* are stored, not as a visibility backend |
| Audit log durability | Namespace retention **1 hour** (the platform floor; 20m proved impossible — see [§6.3](#63-truncation-is-the-feature)); continue-as-new input carries running totals; UI renders as much history as survives plus an explicit truncation notice. `make reap` forces truncation on demand |
| Deactivate | `CancelWorkflow` (graceful), not Terminate |
| Add points | Update (not Signal), so the UI gets synchronous success/failure |
| Continue-as-new | After **3** successful point-add updates (hardcoded; production should use `GetContinueAsNewSuggested()` — see [§3.5](#35-continue-as-new-after-3-updates)) |
| Datastore inspection | An explicit POC goal — tooling and docs for looking inside both stores ([§8](#8-inspecting-postgres-and-elasticsearch)) |
| Activities | Exactly one, `NotifyCustomer` — a stubbed tier-promotion notification ([§3.7](#37-tier-promotion-notifications)) |

Making history truncation *observable* rather than theoretical is the most interesting choice
here — see [§6.3](#63-truncation-is-the-feature). The original plan did that with a 20-minute
retention; Phase 0 established that is not achievable, so `make reap` does it on demand
instead.

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

**All rewards state transitions are pure functions of workflow state** — no Activity is
needed to compute a balance or a tier. That is exactly the "Temporal as a data store"
argument, and worth stating plainly in the README. The single Activity we do have
(`NotifyCustomer`, [§3.7](#37-tier-promotion-notifications)) exists because talking to the
outside world is the one thing a workflow cannot do itself, which is a clean illustration of
where the boundary actually sits.

### Repo layout

```
cmd/worker/main.go            worker process
cmd/api/main.go               HTTP API process
internal/rewards/
  state.go                    CustomerState, tier thresholds, derived Level()
  workflow.go                 CustomerRewardsWorkflow
  workflow_test.go            unit tests via testsuite
  searchattr.go               typed search attribute keys
  activities.go               NotifyCustomer (the only Activity)
internal/history/
  audit.go                    multi-run history crawl → audit entries
internal/httpapi/             handlers, DTOs
web/                          Vite + React + TS
deploy/
  docker-compose.yml          full stack (Postgres + ES)
  dynamicconfig/dev.yaml      retention jitter + visibility-lag overrides
  bootstrap.sh                namespace + search attribute registration
  reap.sh                     force-delete closed executions (§6.3)
  inspect/
    verify-config.sh          platform assumption checks
    pg-*.sql / es-*.sh        canned queries for §8
    write-trace.sh            one addPoints through both stores
docs/
  PLAN.md                     this file
  DATASTORES.md               how Temporal uses Postgres and Elasticsearch
README.md                     quick start and the two surprises
.env.example                  all ports, versions, tuning
Makefile                      up / down / bootstrap / verify-config / reap / psql / es / inspect
```

Operational scripts run via `exec` into the **server** container rather than a separate
`admin-tools` one: the `auto-setup` image already ships the `temporal` CLI, which saves
several seconds per invocation and one more image to pin. It has `bash`, `curl` and `grep`
but no `python3` or `jq`, which constrains how those scripts are written.

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

    Points     int // monotonic — only ever increases, see below

    // Survives history reaping — this is what lets the audit log detect and
    // quantify its own truncation. See §6.3.
    EnrolledAt         time.Time // original enrollment, from the very first run
    LifetimeEarnEvents int       // count of all successful adds, ever
    Generation         int       // how many times we've continued-as-new

    // Levels we've already sent a promotion notification for. Guards against
    // at-least-once Activity delivery re-notifying after a replay. See §3.7.
    NotifiedLevels []string
}
```

#### Points only go up

**Decided:** there is no spending, redemption, expiry, or manual adjustment in this POC, and
none is planned. `addPoints` is the only thing that ever writes `Points`, and it only ever adds.
Balances are monotonic for the life of a customer.

The consequence is that there is no separate lifetime total. An earlier draft carried both a
current `Points` and a `LifetimePoints`, on the theory that spending would eventually make them
diverge. With a monotonic balance they are the same number, always — so carrying both bought
nothing except an invariant to violate. It duly got violated: because the handler's cap was
measured against the caller-supplied `LifetimePoints`, a start payload with a large `Points` and
a zero `LifetimePoints` walked straight past it. One field, one number, no gap to exploit.

`LifetimeEarnEvents` is *not* redundant in the same way and stays: it counts adds, not points,
and §6.3 needs it to quantify how much of the audit log has been reaped.

Two things follow that are worth being explicit about, since both are places a reviewer will
reasonably expect different behaviour:

- **Re-enrollment starts at zero** (§3.6) is the only way a balance ever decreases, and it does
  so by starting a *new* execution rather than by mutating one — the old customer's final
  balance is simply gone with them.
- **Tier demotion cannot happen** through normal operation. Since tiers are derived from a
  monotonic balance (§3.2), a customer's tier is monotonic too. The only way to demote is to
  raise a threshold, which retroactively demotes everyone at once.

If spending ever arrives, it means reintroducing a separate lifetime field — not repurposing
`Points`. The cap, the tier derivation, and the audit log would all need to pick a side.

#### The workflow is the integrity boundary

Worth stating explicitly, because it is the cost of the headline decision. With no application
database there is no schema, no `CHECK` constraint, no `NOT NULL`, and no unique index standing
behind this state. A workflow accepts whatever payload it is started with, so **every invariant
that a table definition would have enforced has to be written by hand**, at the top of the
workflow, or it is not enforced at all.

Phase 1 validates on start: `CustomerID` must match the workflow ID it was started under
(otherwise search attributes and `getStatus` report one customer while every operation is keyed
by another), counters must be non-negative, `Points` must not already exceed the cap, and points
cannot exist without an earn event to have earned them in.

A rejected enrollment fails the execution outright (`WorkflowExecutionFailed`, attempt 1, no
retry loop — verified) rather than producing a running customer whose numbers do not add up.

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
- *"This add would push them past their points cap"* — genuinely useful. A support rep
  asking "why didn't this customer reach platinum?" wants exactly that row.

So we use both phases for what each is good at:

- **Validator** (leaves no history) — `Amount <= 0`, `Amount > MaxPointsPerTxn`, empty reason.
- **Handler returning an error** (both events recorded) — one business rule: reject if the add
  would push `Points` past `PointsCap`.

#### Why this is more than a stylistic choice

Two independent consequences push the same way.

**History bloat and abuse.** We continue-as-new every 3 adds, and history has hard limits
(50k events / 50 MB). Validator rejections are *free* — a buggy retry loop hammering
`amount: -1` cannot grow history at all. Handler rejections are recorded, so every rule
enforced there is a surface for history growth. Keep that set small and meaningful.

**Determinism surface.** Handler code is replayed; validator rejections leave nothing to
replay. If the points cap lives in the handler and we later change the constant, we have
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
        return earnsThisRun >= EarnsPerRun
    }); err != nil {
        // Cancelled — graceful departure. Cleanup, if any, needs a
        // disconnected context because ctx is already cancelled.
        return handleLeave(ctx, &state)
    }
    // Await returns nil the moment its condition holds, without checking
    // the context — so a cancel arriving in the same transition lands
    // here with err == nil. Departure wins. See §12.9.
    if ctx.Err() != nil {
        return handleLeave(ctx, &state)
    }

    // Let any concurrently-accepted update AND any in-flight notification
    // finish before we roll the run. See §3.7 for why the second clause
    // is not covered by AllHandlersFinished.
    if err := workflow.Await(ctx, func() bool {
        return workflow.AllHandlersFinished(ctx) && notifier.Idle()
    }); err != nil {
        return handleLeave(ctx, &state)
    }
    if ctx.Err() != nil {
        return handleLeave(ctx, &state)
    }

    state.Generation++
    return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
}
```

#### A fixed count is the wrong rule, and we use it anyway

`EarnsPerRun = 3` is a hardcoded constant, chosen because three adds is easy to demonstrate in
a terminal — not because it is defensible. What actually bounds a run is history **size**, and
a count of adds is only a proxy for it: three is wastefully early for small updates and would
be far too late if each add carried a large payload. The real limits are 50k events and 50 MB
per run, and neither is a number of adds.

The server already tracks the real thing:

```go
workflow.GetInfo(ctx).GetContinueAsNewSuggested()
```

which flips to true as a run approaches those limits, with
`GetContinueAsNewSuggestedReasons()` reporting which one. **Production should roll on that**,
and the POC should say so at the call site rather than leaving a magic 3 to be cargo-culted.

An earlier version of Phase 2 made the threshold configurable — env var, worker-level setter,
zero meaning "ask the server" — so both behaviours were demonstrable. That was removed. It
bought a demo of the correct behaviour at the cost of a mutable knob on a value that is baked
into recorded history, which is precisely the shape that causes the non-determinism in
[§12.10](#12-sharp-edges). One hardcoded constant with a comment explaining what production
should do instead is both simpler and harder to misuse than a switch inviting people to change
it at runtime.

### 3.6 Deactivation via cancel

`client.CancelWorkflow(ctx, "customer-<id>", "")`. The pending `workflow.Await` returns a
cancelled error, `handleLeave` records the departure and returns `ctx.Err()`, and the
execution closes as `Canceled`.

Cancel, not Terminate: Terminate skips workflow code entirely, so no cleanup runs and the
departure is never recorded by our own code.

Re-enrollment works because the workflow ID is free once the execution closes. A double-create
against a *running* customer should return a clean 409 rather than silently attaching to the
existing run — which takes **two** settings on `StartWorkflowOptions`, not one:

```go
WorkflowIDConflictPolicy:                 enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
WorkflowExecutionErrorWhenAlreadyStarted: true, // without this, ExecuteWorkflow returns nil
```

Verified in Phase 1: with the conflict policy alone, `client.ExecuteWorkflow` returns the
*existing* `WorkflowRun` and a **nil error**, so there is nothing for the API to map to a 409.
Only with the second flag does it return `serviceerror.WorkflowExecutionAlreadyStarted`. The
conflict policy governs what the server does; this flag governs whether the SDK tells you
about it. (`WorkflowIDConflictPolicy` also already defaults to `Fail`, so the flag that looks
redundant is the load-bearing one.)

The `temporal` CLI hides this too: `workflow start --id-conflict-policy Fail` against a
running execution prints `Running execution:` with the existing run ID and **exits 0**. So the
CLI cannot be used to verify this behaviour — check it through the SDK.

**Re-enrolling starts over at zero points and basic tier** — decided, not a default. A
returning customer is simply a fresh enrollment that happens to reuse the workflow ID, so
there is no "restore the prior balance" path to build and no dependency on the old run's
history (which after reaping is usually gone anyway). The UI should say so at
the point of deactivation, since it makes the action irreversible in a way "deactivate"
doesn't imply on its own.

### 3.7 Tier promotion notifications

**Done.** The one Activity in the system. `NotifyCustomer(ctx, NotifyRequest{CustomerID, Email, Event,
Level})` logs a line saying what would be sent, and returns. The body is a stub — production
would call an email or push service here — but everything *around* it is real, and that is
the part worth building.

Triggered when a point-add leaves the customer at a tier they have not been told about. Because
tiers are derived (§3.2) this is a pure comparison inside the handler: is `Level(points)` absent
from `NotifiedLevels`?

**One notification per customer per tier reached, naming where they are now — not one per
threshold passed.** `MaxPointsPerTxn` is 1000 and platinum starts at 1000, so a single add from
zero lands a customer in platinum having never been observed at gold, and only platinum is
announced. Sending "Welcome to Gold" beside "Welcome to Platinum" would describe a state that
held for no measurable time. The original crossing rule behaved identically; it is a decision,
not a consequence of the retry rewrite.

**Not** "did this add cross a boundary", which is what this said first and is subtly wrong: a
crossing is an event that happens once, so a delivery that failed could never be reattempted.
See [§12.33](#12-sharp-edges).

#### Where it runs, and why not in the handler

The tempting thing is to await the Activity inside the `addPoints` handler, so the Update
does not return until the customer has been notified. Don't:

- **It couples two operations with different failure semantics.** The points were legitimately
  earned and are already recorded in history. A notification service being down must not fail
  the point-add or roll it back — but an awaited Activity error inside the handler does
  exactly that.
- **It puts a network call on the UI's critical path.** With a default retry policy an
  unreachable notifier would retry indefinitely, so the Update hangs until the client times
  out — with the points still awarded. The worst possible UX for the clearest possible reason.

So the handler stays synchronous and cheap: it applies the points, detects the crossing,
appends to a pending-notification slice, and returns the new balance immediately. A
`workflow.Go` goroutine drains that slice and runs the Activity:

```go
workflow.Go(ctx, func(gctx workflow.Context) {
    for {
        if err := workflow.Await(gctx, func() bool { return len(pending) > 0 }); err != nil {
            return // cancelled
        }
        n := pending[0]
        pending = pending[1:]
        inFlight = true
        _ = workflow.ExecuteActivity(actCtx, NotifyCustomer, n).Get(gctx, nil)
        state.NotifiedLevels = append(state.NotifiedLevels, n.Level)
        inFlight = false
    }
})
```

#### The trap this creates

**`workflow.AllHandlersFinished` does not cover `workflow.Go` goroutines.** It tracks Update
and Signal handlers only. So the pre-continue-as-new await in §3.5 would happily roll the run
while a notification is still in flight, silently dropping it — and at `EarnsPerRun = 3`, a
promotion landing on the third add is exactly when that happens. Hence the extra
`notifier.Idle()` clause (`len(pending) == 0 && !inFlight`).

This is the most instructive bug in the whole design, and it is worth writing the test that
catches it before writing the fix.

**It was, and it failed.** `Test_Notify_PromotionOnTheRollingAddIsNotDropped` reported no
notifications at all against the unguarded workflow — see [§12.6](#12-sharp-edges) for the
output. Adding `&& n.idle()` to the pre-roll await turned it green.

Two things the sketch above leaves out, both learned building it:

- **The goroutine runs on a disconnected context.** On the workflow's own context, deactivating
  a customer mid-delivery cancels a promotion they had already earned — and then `handleLeave`
  waits for a drain that can never complete, because the goroutine died with the context. A
  promotion is not unmade by the customer leaving a moment later.
- **The Activity's retries must be bounded.** The default policy retries forever, and the
  continue-as-new guard waits for the notifier to go idle, so an unreachable notification
  provider would stop the customer's workflow rolling for as long as it stayed down. A cosmetic
  outage would become a stuck entity workflow. `MaximumAttempts: 3`.

#### At-least-once, and what that means here

Activities are at-least-once: a worker crash after `NotifyCustomer` runs but before its
completion is recorded means it runs again on replay. For a real email that is a duplicate in
someone's inbox.

Two mitigations, both cheap:

- Carry `NotifiedLevels []string` in `CustomerState` (so it survives continue-as-new) and skip
  any level already in it. This is the workflow-side guard, and it is also the retry ledger: a
  level stays out of it until a delivery actually succeeds, so a failed one is re-offered by
  the next add.
- Pass an idempotency key of `<customerID>:<level>` to the Activity, since a customer reaches
  gold exactly once. This is the guard a real notification service would honour, and including
  it in the stub documents the contract even though the stub ignores it.

#### It lands in the audit log for free

`ActivityTaskScheduled` / `ActivityTaskCompleted` are history events, so the §6 crawl picks
them up with no extra work — the customer detail page gets "Promoted to gold — notification
sent" rows interleaved with the point-adds. That is a nice demonstration that the audit log
reflects *everything* the workflow did, not just the parts we explicitly designed it around.

It really was free, but only because Phase 5 declared `NotifyRequest` and
`ActivityNotifyCustomer` a phase early so the crawl could compile against the real shape rather
than guess at one. Registering the Activity was the whole integration: the rows appeared with
no change to `internal/httpapi/audit.go`.

One row it does *not* produce is the departure notice — see [§12.31](#12-sharp-edges).

#### Reuse for departure

`handleLeave` (§3.6) sends the same Activity with `Event: "departed"`, which removes the need
for the separate cleanup Activity the plan previously carried. It runs on a disconnected
context, since by then `ctx` is already cancelled — a compact demonstration of why
`workflow.NewDisconnectedContext` exists.

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

### Visibility indexes Runs, not customers

**One document per Run, not per Workflow ID.** A customer who has continued-as-new twice has
*three* visibility documents, each frozen at that generation's balance. Anything that treats a
visibility result set as a customer list is therefore double-counting, and does it silently —
every individual row is correct, the totals are just wrong:

```
WorkflowId = 'customer-dup-check'                           Total: 3
WorkflowId = 'customer-dup-check' AND status != CAN         Total: 1
```

Every list and count must exclude rolled-over generations:

```
ExecutionStatus != 'ContinuedAsNew'
```

That leaves exactly the current generation whatever its final state — `Running` for an active
customer, `Canceled` for a departed one, `Failed` for an enrollment that never validated.
`IN ('Running','Canceled')` looks equivalent and silently drops that last group: 45 against 47
on the same data.

This shipped as a bug in the Phase 4 list endpoint and was caught by the Phase 7 datastore
inspection — which is the argument for [§8](#8-inspecting-postgres-and-elasticsearch) being a
deliverable rather than a nice-to-have. No API test would have found it, because the API was
faithfully reporting what it was asked for.

This powers the customer list page's **filtering**:
`RewardsLevel = "gold" AND ExecutionStatus = "Running"`.

**It cannot do the sorting.** `ORDER BY` is rejected outright by server 1.29.7 —
`ORDER BY clause is not supported` — and not just for custom attributes: it is refused for
built-ins like `StartTime` and `CloseTime` too, with Elasticsearch visibility active and
custom search attributes otherwise working normally. Verified in Phase 1 against the real
stack, so this is the platform's answer, not a misconfiguration.

What still works, and is enough: equality and range filters on custom attributes
(`RewardsPoints >= 500`), `Text` partial match on `CustomerName`, `Keyword` exact match on
`CustomerEmail`, and `ExecutionStatus` for active-vs-deactivated. Results come back in the
server's default order (most recent first).

The consequence lands on Phase 4 and Phase 8: **sorting must happen client-side**, which is
only equivalent to server-side sorting when the full filtered result set fits in one page.
Sorting a page of an arbitrarily-paginated list sorts the wrong thing. For a POC with tens of
customers, fetch the filtered set and sort in the API. Worth stating plainly in the README,
because "sort the customer list by points" is the first thing anyone will ask for and the
honest answer is that Temporal's visibility store is a filter index, not a reporting database.

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
POST   /api/customers              → ExecuteWorkflow (conflict policy FAIL)     Phase 3
GET    /api/customers?q=<sql>      → ListWorkflow + CountWorkflow                Phase 3
GET    /api/customers/{id}         → QueryWorkflow(getStatus) + Describe        Phase 3
POST   /api/customers/{id}/points  → UpdateWorkflow(addPoints), synchronous     Phase 3
DELETE /api/customers/{id}         → CancelWorkflow                             Phase 3
GET    /api/customers/{id}/audit   → history crawl (§6)                         Phase 5
```

The API holds a Temporal Client and nothing else — no database, no cache, no ORM. That is
the whole argument of the POC and the code should make it obvious at a glance.

### 5.1 The contract is frozen ahead of the endpoints

`CustomerListItem`, `CustomerListResponse`, `AuditEntry` and `AuditResponse` are defined in
`internal/httpapi/dto.go` **before** the endpoints that return them, so the UI (Phase 8) can be
built in parallel with Phases 4 and 5 rather than serialised behind them. `cmd/mockapi` serves
that contract from fixtures:

```sh
make mockapi     # :8082, no Temporal, no Docker, no .env
```

Sharing the DTOs means the mock cannot drift from the real API without failing to compile.

Two shape decisions are worth flagging, because both encode a platform constraint rather than a
preference:

- **`CustomerListItem` is deliberately narrower than `CustomerResponse`.** The list is served by
  `ListWorkflow`, which returns *search attributes only*, so `LifetimeEarnEvents` is absent by
  construction rather than by omission.
- **There is no pagination.** The list returns at most `ListLimit` (5) customers, reports how many
  matched in total, and tells the user to filter. This follows from `ORDER BY` being rejected
  ([§12.8](#12-sharp-edges)): with no stable ordering, "page 2 of customers" does not mean
  anything in particular, so paging would be machinery that cannot be made coherent. Filtering is
  what the visibility store is actually good at, so the UI pushes people towards it.

  The total comes from `CountWorkflow`, which is a separate API from `ListWorkflow` and — verified
  against 1.29.7 — honours the same filters:

  ```
  WorkflowType = 'CustomerRewardsWorkflow'                          count=58
  WorkflowType = 'CustomerRewardsWorkflow' AND ExecutionStatus = 'Running'  count=36
  RewardsLevel = 'gold'                                             count=13
  ```

  So the notice is an exact "Showing 5 of 23 — filter to find additional results". `Total` is
  `-1` if the count call fails, which degrades the message to "of many" rather than failing the
  list. Being two queries, `Total` and `Items` can disagree by a row under concurrent writes;
  that is not worth solving for a number displayed next to the word "filter".

  Two things the UI must not assume: *which* five it gets is unspecified and can differ between
  identical requests, and sorting them is only sorting the real set when `Complete` is true —
  otherwise it is sorting a sample.

The fixtures cover the cases a UI built against a freshly-enrolled happy path gets wrong: a
top-tier customer with no next tier, a customer with an empty timeline, a deactivated customer,
a truncated audit log, a customer sitting under the points cap so handler rejections are
reachable, and the ~400 ms visibility lag on newly created customers.

**A closed execution answers Queries perfectly well.** Temporal replays its history to serve
them, so a deactivated customer returns full state — balance, tier, `LifetimeEarnEvents`, the
lot. Worth stating because assuming the opposite is easy and the cost is silent: Phase 3
initially short-circuited on status and fell straight to search attributes, which carry no
`LifetimeEarnEvents`, so departed customers read back missing a field that had been available
all along. The assumption was never tested because the code path that would have tested it was
skipped. Corrected once measured.

The search-attribute fallback survives, on the degraded path only: replay needs a worker, so a
closed customer with none would otherwise 503 despite the execution record already holding most
of the page. Falling back beats failing for someone who cannot change anyway.

It does **not** cover a reaped customer. `make reap` deletes the whole execution record,
search attributes included, so those fail at `Describe` and surface as a 404 rather than
degrading — see [§6.3](#63-truncation-is-the-feature). Truncation detection is the §6 crawl's
job, not this endpoint's.

Error mapping worth getting right, because these are the failure modes a reviewer will poke
at:

| Condition | HTTP | How it is actually detected |
|---|---|---|
| Malformed or incomplete request | 400 | validated in the handler, before Temporal |
| Update rejected by validator or handler | 422 + message | `*temporal.ApplicationError` — see below |
| Customer already exists and is running | 409 | `*serviceerror.WorkflowExecutionAlreadyStarted` |
| Workflow not found / history reaped | 404 | `*serviceerror.NotFound` |
| No worker polling | 503 + "worker unavailable" | `FailedPrecondition`, `DeadlineExceeded`, or our own timeout |
| Update lost to a continue-as-new race | retried once, then 409 | best-effort; see [§12.10](#12-sharp-edges) |

**Both halves of the validator/handler split arrive as `*temporal.ApplicationError`**, and are
told apart only by `Type()`: a handler rejection carries the type we chose
(`PointsCapExceeded`), a validator rejection carries an empty one. That is more convenient
than expected — no message matching is needed to separate a business rejection from an outage,
which was the thing most likely to go wrong here. Both map to 422 carrying `appErr.Message()`,
which excludes the SDK's `(type: ..., retryable: ...)` suffix. The caller cannot tell the two
apart, which is the intent of [§3.4](#34-validation--and-a-deliberate-split), not a limitation.

**Timeouts are load-bearing on both read paths**, and were sized from measurement rather than
taste:

- A **Query** with no worker takes ~9–10s to fail, and *how* it fails is not stable: within a
  few seconds of the worker dying it is a bare gRPC `RST_STREAM` (a 500), later it is
  `FailedPrecondition` or `DeadlineExceeded`. A 2 s bound means our own deadline usually wins
  the race and all three collapse into one predictable 503 — against ~30 ms for a healthy
  query, so the headroom is ~60×.
- An **Update** with no worker does not fail at all. It *blocks* — observed still waiting after
  two minutes. Without a bound, `POST /points` hangs for as long as the client will hold the
  connection whenever the worker is down. 15 s.

The asymmetry is worth internalising: the same underlying condition fails fast on one API and
never on the other.

---

## 6. Audit log by crawling history

The interesting part, and the reason for the 3-update continue-as-new.

### 6.1 Walking the run chain

**Done.** A customer's life spans many Runs sharing one Workflow ID. Newest run first, walk
backwards:

1. `DescribeWorkflowExecution(workflowID, "")` → the current run.
2. `GetWorkflowHistory(workflowID, runID)` → events for that run.
3. The first event, `WorkflowExecutionStarted`, carries `ContinuedExecutionRunId`. Non-empty
   means there is a previous run; recurse. Empty means this is the original enrollment run
   and the chain is complete.
4. Reverse to get oldest-first.

Two details this leaves out, both discovered building it:

- **Every run is addressed by an explicit run ID, never by `""`.** Step 2 above is the only
  place the empty run ID would be convenient, and using it there breaks truncation detection:
  the server resolves `""` to the latest run, so a request for a reaped predecessor would
  cheerfully return the *newest* run's events instead of reporting the predecessor as gone.
  Step 1 exists purely to bootstrap a real run ID for step 2.
- **`isLongPoll` must be false.** With it true the iterator on a running workflow blocks
  waiting for events that have not happened yet, so the audit page for an active customer
  hangs rather than returning what exists now.

**The crawl needs no worker.** Slightly surprising, since it is by far the most expensive read
in the system, but it only ever talks to the server: history and the carried state come out of
persistence, not out of a replay. Measured with `make worker-stop`, `GET /audit` answers in
10 ms while `GET /api/customers/{id}` — which needs a Query — 503s. The one page that looks
like it should be fragile is the most available thing here.

### 6.2 Events we care about

| Event | Audit entry |
|---|---|
| `WorkflowExecutionStarted` (with empty `ContinuedExecutionRunId`) | Enrolled |
| `WorkflowExecutionStarted` (with a non-empty one) | generation boundary (render as a subtle divider) |
| `WorkflowExecutionUpdateAccepted` | the request: update name, `Amount`, `Reason`, update ID |
| `WorkflowExecutionUpdateCompleted` | the outcome: new balance and level, or failure message |
| `ActivityTaskScheduled` + `ActivityTaskCompleted` | Promotion notification sent (§3.7) |
| `WorkflowExecutionCancelRequested` | Deactivated |

Accepted and Completed are separate events; pair them via `AcceptedEventId` on the completed
event to render one row with both the request and its result. Payloads are `Payloads` protos
— decode with the same `DataConverter` the client is configured with, which is why API and
worker share a Go module.

Three corrections to the table as originally written:

- **The generation boundary is read from the successor, not the predecessor.** The plan listed
  `WorkflowExecutionContinuedAsNew`, which is the last event of the run being *left*. Both
  events mark the same instant, but only the successor's `WorkflowExecutionStarted` knows which
  generation is being entered — and, more to the point, the predecessor is the half that gets
  reaped. Reading the boundary from the run that still exists is what lets a truncated log
  still show "generation 33" at the top instead of starting mid-air.
- **`WorkflowExecutionStarted` carries the whole carried `CustomerState` as its input**, which
  turns out to be the most useful event in the crawl. It gives the generation and
  `LifetimeEarnEvents` for free, which is where §6.3's "3 of 21" number comes from — no Query
  needed, and therefore no worker.

  Note the search attributes on that same event are the *predecessor's* — the workflow upserts
  its own on its first task. Observed a `RewardsGeneration` of 1 sitting on the started event
  of the run whose input said generation 2. Read the input, not the attributes.
- **A notification row needs `ActivityTaskCompleted`, not `ActivityTaskScheduled`.** "Sent"
  has to mean sent: an Activity that exhausted its retries leaves a Scheduled event and a
  Failed one, and rendering the first as a delivery would make the audit log lie about the one
  thing it exists to be believed about. The Scheduled event carries the input, the Completed
  one carries `ScheduledEventId` — so pair them exactly like the Update halves above.

§3.7 says these rows land "for free". They do not land *quite* for free: the crawl has to know
the Activity's name to tell a notification from any other Activity a later phase might add, and
has to decode its argument to get the level. `ActivityNotifyCustomer` and `NotifyRequest` are
therefore declared in `internal/rewards/notify.go` a phase early, so Phase 6 cannot change the
shape without the crawler failing to compile. Same trade as `CustomerState.NotifiedLevels`.

### 6.3 Truncation is the feature

Closed runs get reaped, and the audit log is designed around that rather than pretending it
won't happen:

- Walking back, a non-empty `ContinuedExecutionRunId` whose `GetWorkflowHistory` fails means
  history was reaped. Stop and mark the result truncated.

  **It is not `NotFound`, which is what this section originally said**, and getting that wrong
  is not cosmetic — truncation is detected *by this error*, so with the predicted
  classification the one case §6.3 exists to handle came back as an unmapped 500. Measured:

  | condition | Go type | message |
  |---|---|---|
  | run reaped | `*serviceerror.InvalidArgument` | *Requested workflow history not found, may have passed retention period.* |
  | run ID well-formed, never used | `*serviceerror.InvalidArgument` | identical to the above |
  | run ID malformed | `*serviceerror.InvalidArgument` | *Invalid RunId.* |
  | workflow ID never existed | `*serviceerror.NotFound` | *workflow not found for ID: …* |

  So the type alone cannot decide it, and `isHistoryGone` is the one place in the codebase
  that matches on message text — everywhere else that would be a bug. There is no other
  signal: from the server's side, "history deleted" and "run ID you made up" are the same
  situation. That is only tolerable because the crawl exclusively passes run IDs the server
  itself produced in a `ContinuedExecutionRunId`, which makes the second row unreachable.

  Note the server says *may have passed retention period* even for a run deleted explicitly by
  `make reap`, where that guess is simply wrong. If a server upgrade changes the wording,
  truncation stops being recognised and surfaces as a 500 — the right direction to fail in,
  since a loud error beats a timeline quietly showing fewer rows than the customer has.
- The carried `CustomerState` gives us ground truth to *quantify* the gap. If
  `LifetimeEarnEvents` is 23 and we could only reconstruct 7 rows, the UI says:

  > Showing 7 of 23 point events. Earlier history has been deleted.

- `EnrolledAt` and the running totals survive in the continue-as-new payload, so the header
  of the detail page stays fully correct even when the log beneath it is partial.

This is the honest version of "Temporal as a data store," and demonstrating the limitation is
more valuable than hiding it.

#### How we trigger it: `make reap`, not a short retention

The original plan set namespace retention to 20 minutes so reaping happened while you watched.
**Phase 0 established that this is not achievable**, and the finding is worth recording
because it is not what the documentation implies:

- Temporal enforces a **1 hour minimum** namespace retention. Probing server 1.29.7 directly,
  `59m` is rejected with "A valid retention period is not set on request" and `1h` is accepted.
- The dynamic config key that would lower the floor, `system.namespaceMinRetentionLocal`,
  **exists only on unreleased `main`**. Setting it on 1.29.7 produces
  `dynamic config warning ... unregistered key "system.namespaceMinRetentionLocal"` at
  startup, after which it is loaded and then ignored — so it looks configured but does
  nothing. 1.29.7 is the newest published `auto-setup` image, so no released server has it.

So retention sits at the 1 h floor, and truncation is forced on demand instead:

```sh
make reap                      # every closed execution in the namespace
make reap WF=customer-abc123   # just that customer's closed runs
```

This is implemented with `temporal workflow delete --query`, a server-side batch operation.
Verified end to end: a closed execution stops resolving roughly 25–40 s after the request, so
deletion is asynchronous and a demo should expect a short pause rather than an instant change.

**The `ExecutionStatus != "Running"` filter in that query is load-bearing, not a nicety.**
`workflow delete` *terminates* a running execution before deleting it, so an unfiltered reap
would destroy every active customer rather than just their old generations.

Arguably this is the better demo anyway: it makes truncation a thing you *do* on cue rather
than a thing you wait out, and it targets one customer so the contrast between a truncated and
an intact audit log is visible side by side.

### 6.4 Consequences to expect

- **Deactivated customers eventually vanish from the list**, at the 1 h mark or on the next
  reap. Their workflow is closed, so it gets reaped and the detail page starts returning 404.
  Running executions are exempt, so this only affects deactivated customers — and it is why
  the reap query filters on status.
- The reaping timer is jittered, so natural deletion lands somewhere in
  `[retention, retention + jitter]` rather than exactly on the hour.
  `history.retentionTimerJitterDuration` **is** a registered key (verified against the 1.29.7
  binary and by the server logging it applied), and dev config pins it to 1m.
- `make verify-config` asserts all of the above — the 1 h floor, that every key in
  `dynamicconfig/dev.yaml` is genuinely registered, and that on-demand deletion works. Re-run
  it after a server upgrade; if a future version relaxes the floor, check 1 fails loudly and
  we can drop the workaround.
- Cost: the crawl is O(runs × events) per page view, uncached, and serial — each run only
  learns its predecessor from the run just read, so the round trips cannot be issued in
  parallel. **Measured, and cheaper than expected**: a customer with 34 runs and 100 point-adds
  crawls end to end in ~125 ms, warm or cold. Fine at POC scale; note pagination and a cache as
  future work rather than building them now.

  There is deliberately **no cap on runs walked**, only a 30 s deadline on the whole request. A
  cap would have to report its partial result as `truncated`, which in this contract means
  "history was deleted" — and quietly widening that to "or we gave up" would make the one
  honest signal in the response dishonest.
- What a reaped customer looks like from the API, measured end to end:

  | state | `GET /{id}` | `GET /{id}/audit` |
  |---|---|---|
  | active, intact history | 200 | 200, `truncated: false` |
  | active, old generations reaped | 200 | 200, `truncated: true`, e.g. shown 1 of 100 |
  | deactivated, not yet reaped | 200 | 200, ending in a `deactivated` row |
  | reaped entirely | 404 | 404 |

  The middle row is the demo: `make reap WF=customer-x` on an *active* customer leaves the
  running generation and deletes every closed one, so the header still reads 100 lifetime
  point-adds while the timeline beneath it can only show the one that survives.

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
TEMPORAL_RETENTION=1h

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

Temporal creates **one** database here: `temporal`, which is persistence. An earlier draft of
this section said `temporal_visibility` also exists with a schema but no rows — that describes a
Postgres-visibility deployment. With `ENABLE_ES=true` auto-setup does not create it at all, so
persistence really is the only thing Postgres is doing for us. Measured in Phase 7.

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
   closed run's rows disappear from `history_node` after a `make reap` (or an hour). That is the
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
query again after `make reap`. ES does not decide to delete that document
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

Vite + React + TypeScript in `web/`. **Done** — three screens against `make mockapi`
(and the same base URL against `make api`).

- **Customer list** — table backed by `ListWorkflow`, capped at five rows with no pagination
  ([§5.1](#51-the-contract-is-frozen-ahead-of-the-endpoints)). Tier filter chips, a status toggle
  (Running/Canceled via `ExecutionStatus`), and a raw search-attribute query box so we can show
  the same query working in the Temporal UI. This is where search attributes earn their keep —
  and where the design has to be honest that filtering, not browsing, is what the visibility
  store supports.

  When more matched than fit, render **"Showing 5 of 23 — filter to find additional results"**
  (or "of many" when `Total` is `-1`). Sorting is offered only when `Complete`; otherwise it
  would sort five arbitrary rows out of twenty-three and look authoritative while being a
  sample.
- **Create customer** — name + email + `customerId` (auto-slugged from the name, editable),
  POSTs, redirects to detail. The ID field is required by `EnrollRequest` even though an earlier
  draft of this section omitted it.
- **Customer detail** — tier badge, points, progress bar to next tier (`nextTierAt == 0` means
  none — do not divide by it), enrollment date; an add-points form showing synchronous success
  or the rejection message (hidden when deactivated); a Deactivate button with confirmation and
  an explicit note that re-enrollment starts at zero; and the audit timeline with generation
  dividers, notification rows, and the truncation notice
  ("Showing N of M point events. Earlier history has been deleted.").

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
(§8.1) will show why when it isn't. The mock reproduces the lag at 400 ms.

Run: `make mockapi` then `make web` (Vite on `:5173`; `/api` proxied to the mock).
`VITE_API_PROXY_TARGET=http://localhost:8081 make web` points at the real API — the Go
server does not send CORS headers, so a cross-origin `VITE_API_BASE` fails in the browser.
Findings for §12 live in `web/NOTES.md`.

---

## 10. Testing

- **Unit** (`testsuite.WorkflowTestSuite`): tier boundaries at 499/500/999/1000; validator
  rejections; handler-side business rejection; continue-as-new fires after exactly 3 adds and
  carries state correctly; cancellation path. Note that the Go test environment needs
  `env.RegisterDelayedCallback` + `env.UpdateWorkflow` to drive updates.
- **The §3.7 race specifically**: a promotion landing on the third point-add must still be
  notified, not dropped by the continue-as-new. Mock `NotifyCustomer` with a delay so the
  Activity is genuinely in flight when the run tries to roll, and assert it was called.
  Without the `notifier.Idle()` guard this test fails — write it first.
- **Replay test** against a checked-in history JSON. **Done, and it earned its place
  immediately**: it caught Phase 6 as a change that would have wedged every existing customer
  ([§12.11](#12-sharp-edges)). `internal/rewards/testdata/pre-notification-*` are histories
  recorded by the Phase 5 worker, so replaying them is a rehearsal of that deploy. Note the
  replayer needs `OriginalExecution` or it invents a workflow ID and fails misleadingly
  ([§12.35](#12-sharp-edges)).
- **Integration**: `temporal server start-dev` with `--search-attribute` flags, exercising
  the API end to end. Runs in CI without Docker or Elasticsearch — note that `start-dev` uses
  SQLite visibility, so it will not reproduce the ES lag of §7.5. That behaviour needs the
  real stack.

---

## 11. Phases

| # | Deliverable | Notes |
|---|---|---|
| 0 | Compose stack, dynamic config, bootstrap, Makefile, `.env.example`, `make verify-config` | **Done.** 20m retention proved impossible; 1h floor plus `make reap` instead — §6.3 |
| 1 | Workflow + worker: enroll, `addPoints` update, `getStatus` query, cancel, tier derivation, unit tests | Drive it entirely from `temporal` CLI before any UI exists |
| 2 | Continue-as-new after 3 adds, carrying totals | **Done.** Includes the `AllHandlersFinished` guard, though it is unfalsifiable until Phase 6 gives a handler something to block on |
| 3 | Go HTTP API + error mapping | **Done.** Error shapes captured against a real server, not guessed — several plan assumptions were wrong; see [§5](#5-http-api) |
| 4 | Search attributes end to end; list + filter | **Done.** Same query verified identical in the API and the Temporal CLI |
| 5 | History crawl + truncation detection | **Done.** §6.3 predicted the wrong error type for a reaped run, which had truncation surfacing as a 500 — see [§6.3](#63-truncation-is-the-feature) |
| 6 | `NotifyCustomer` Activity: tier-crossing detection, async drain goroutine, CAN-drain guard, `NotifiedLevels` dedup, departure reuse (§3.7) | **Done.** The dropped-notification test was written first and did fail — [§12.6](#12-sharp-edges). Audit rows appeared with no change to the Phase 5 crawl |
| 7 | Datastore inspection: `DATASTORES.md`, `deploy/inspect/`, `make psql` / `make es`, the end-to-end write trace | **Done.** Findings for §12 in `docs/DATASTORES.md` ("Findings for PLAN.md") — integrator to splice |
| 8 | React UI, all three screens | **Done.** Built against `make mockapi`; see `web/` and `web/NOTES.md` |
| 9 | Replay test, seed script, README | **Done.** The replay test caught Phase 6 as a breaking change for every running customer, fixed with `GetVersion` — [§12.11](#12-sharp-edges) |

Phases 1–2 are the substance and Phase 7 is the other headline deliverable; the rest is
scaffolding around them. Phase 6 slots in after the history crawl so the notification events
show up in an audit log that already works.

---

## 12. Sharp edges

Things not in the original brief that will come up.

**Platform behaviour**

1. Continue-as-new inside an Update handler is unsupported — must live in the main function.
2. An Update in flight when a run rolls over gets aborted; the client must retry against the
   new run. With `EarnsPerRun = 3` this race is *frequent* — every third add can hit it. The
   API must retry transparently or the demo looks broken.

   **The error that reports it is ambiguous, and getting this wrong is easy.** An Update whose
   run has closed comes back as `*serviceerror.NotFound` carrying *"workflow execution already
   completed"* — and that is the same error, byte for byte, whether the run closed because it
   continued-as-new or because the customer deactivated. It has to be: in both cases the run
   really did complete. The two need opposite responses (retry vs refuse), so **the error alone
   cannot decide**. Ask the server what is running now: a successor execution means a rollover,
   no open execution means a departed customer. Matching the message instead shipped
   "please retry" to callers adding points to a deactivated customer, where retrying can never
   succeed — caught by review on PR #6.

   **And "operate on a closed execution" is not one behaviour — it depends on the operation.**
   Measured on 1.29.7:

   | on a closed execution | `Canceled` | `Failed` | `Terminated` | never existed |
   |---|---|---|---|---|
   | `CancelWorkflow` | `nil` | `nil` | `nil` | `NotFound` |
   | `UpdateWorkflow` | `NotFound` | — | — | `NotFound` |

   Cancel is idempotent server-side; Update is not. So `DELETE` needs no disambiguation and is
   naturally idempotent, while `POST /points` needs the extra `Describe` above. Assuming the two
   behave alike is a reasonable guess and a wrong one — it was raised as a bug against `DELETE`
   on PR #6 and disproved by measurement.
3. Update dedup via `UpdateID` is scoped to a single run, so it does **not** survive
   continue-as-new. A retry that straddles a rollover can double-apply. The UI should send a
   UUID per click, and we should document that per-run dedup is not sufficient for real money.
4. **No worker polling fails differently depending on which API you call, and on how long the
   worker has been gone.** A Query fails in ~9–10s, as a bare gRPC `RST_STREAM` in the first
   seconds and as `FailedPrecondition` or `DeadlineExceeded` later. An **Update does not fail
   at all** — it blocks indefinitely, observed still waiting after two minutes. Both need a
   client-imposed timeout to become a usable 503; measurements and chosen bounds in
   [§5](#5-http-api). During development the worker will be down often, so this is the failure
   mode a reviewer is most likely to meet first.
5. `workflow.Now()`, never `time.Now()`. Same for randomness and UUIDs.
6. **`AllHandlersFinished` covers Update and Signal handlers, not `workflow.Go` goroutines.**
   Any background work spawned that way needs its own drain condition before continue-as-new
   or workflow completion, or it is silently dropped ([§3.7](#37-tier-promotion-notifications)).

   **Confirmed in Phase 6, and it is not a corner case.** Written as a test before the guard
   existed, exactly as [§10](#10-testing) asks: three adds where the third crosses into gold —
   the ordinary shape at `EarnsPerRun = 3` — and the promotion vanished.

   ```
   --- FAIL: Test_Notify_PromotionOnTheRollingAddIsNotDropped
       expected the promotion to survive the roll, got []
   ```

   No error, no retry, no trace in history: the run rolled while the notification sat in a
   queue that `AllHandlersFinished` knows nothing about. Adding `&& n.idle()` to the pre-roll
   await fixed it. The same clause is needed in the departure path for the same reason.
7. **A duplicate start is silent by default.** `WorkflowIDConflictPolicy: FAIL` is not enough
   to make `ExecuteWorkflow` return an error — it also needs
   `WorkflowExecutionErrorWhenAlreadyStarted: true`, or it returns the existing run and a nil
   error. The `temporal` CLI hides it as well, exiting 0. Confirmed in Phase 1;
   see [§3.6](#36-deactivation-via-cancel).
8. **`ORDER BY` is not supported in visibility queries**, for custom *or* built-in attributes,
   even on Elasticsearch visibility. Filtering is server-side; sorting is the caller's problem,
   and is only correct when the whole filtered set fits one page. Confirmed in Phase 1 on
   server 1.29.7; see [§4](#4-search-attributes).
9. **`workflow.Await` returning `nil` does not mean "not cancelled."** It evaluates its
   condition *before* it checks the context:

   ```go
   for !condition() {
       ... return NewCanceledError(...) if ctx is done ...
       state.yield("Await")
   }
   return nil
   ```

   so a condition that already holds short-circuits the cancellation check entirely. Any
   `Await` whose nil return is treated as "the condition fired, and only the condition" is
   wrong whenever cancellation can race it. In §3.5 that meant a cancel arriving in the same
   workflow task as the Nth point-add would roll the run instead of deactivating — and strand
   the departure permanently, because continue-as-new starts a fresh run while the cancellation
   targeted the run that just ended. The customer clicks deactivate and stays active. Re-check
   `ctx.Err()` after every such `Await`. Found by review on PR #5.
10. **Continue-as-new races the read path too, not just the write path.** Item 2 anticipates an
    Update being aborted by a rollover. A **Query** immediately after a point-add that triggered
    one can also fail, with `Workflow task is not scheduled yet.` — the successor run exists but
    has no workflow task to dispatch the query to. Observed once through the API at three adds
    per run, transient, and not reproducible on demand despite deliberate attempts. The API
    retries unrecognised query failures once rather than matching that message, on the grounds
    that a ~30 ms idempotent read is cheap to repeat and the error could not be pinned down.
    Recorded here because the *class* is real even though this instance is not reliably
    reproducible, and because "not scheduled yet" will look like a bug to whoever meets it next.

    A related caution from the same phase: **the update-side rollover retry has never been
    observed firing.** Sequential adds complete before the next begins, so the roll finishes in
    the gap and no update is ever in flight across it. The retry is now correct *if* it fires —
    it disambiguates via Describe rather than by message, per item 2 — but "correct and
    unexercised" is what it is, and the same was true of two guards in §3.5. Phase 8's UI, which
    can issue overlapping adds from a browser, is the first thing likely to exercise it.

**Operational**

11. **Versioning is the real risk.** Entity workflows run forever and will outlive deploys;
   changing workflow code under a running execution causes non-determinism errors.

   **Confirmed in Phase 9, and it had already happened.** Phase 6 shipped a change that would
   have wedged every customer with an open run. Adding the notification Activity emits a
   `ScheduleActivityTask` command that histories recorded by the Phase 5 worker have no event
   for, so replay fails, the workflow task retries forever, and the customer is stuck — with no
   error surface anywhere a user or an operator would look:

   ```
   nondeterministic workflow: extra replay command for ScheduleActivityTask:
     (ActivityType:(Name:NotifyCustomer) ...)
   ```

   Nothing in Phases 6–8 caught it. Unit tests pass, the API works, the UI renders; the damage
   only exists for executions that started *before* the deploy, and the only thing that looks at
   those is a replay test. That is the whole argument for [§10](#10-testing) insisting on one.

   Fixed with `workflow.GetVersion` gating the Activity, so runs recorded before the marker keep
   the old behaviour for the rest of their lives and pick notifications up at their next
   continue-as-new — at most `EarnsPerRun` adds away. `make reset` is still worth having for
   dev, but it is a convenience, not the answer.
12. Customer names and emails land in Event History and are readable in plaintext in the
   Temporal UI. Fine for a POC; the production answer is a Codec Server. Say so explicitly,
   and use obviously fake seed data.
13. No authentication anywhere. State it in the README so nobody mistakes this for a starting
   point for something exposed.
14. Elasticsearch and Temporal server versions must be compatible (ES 7 needs Temporal 1.7+,
   ES 8 needs 1.18+). Pin both images.
15. ES visibility lag is tunable to ~200–300 ms but never to zero ([§7.5](#75-cutting-the-visibility-lag)).
    Anything needing read-after-write must go through Query or Describe, not `ListWorkflow`.
16. **Stale processes are invisible and look exactly like a code bug.** `go run` execs its
    binary out of `/root/.cache/go-build/<hash>/worker` — a path containing neither `cmd/worker`
    nor `exe/worker` — so the obvious `pkill -f cmd/worker` leaves it alive and happily polling
    the same task queue with the *old* code. Cost real debugging time in Phase 2: continue-as-new
    was correct and unit-tested, but a pre-Phase-2 worker kept winning the tasks, so `generation`
    stayed 0 and the feature looked broken. Use `make worker-stop` (which matches the cache path)
    and `make workers` to confirm exactly one is running before concluding anything about
    workflow behaviour. Worth pairing with the versioning discipline in item 11: stale *workflows*
    and stale *workers* fail in opposite directions — one errors loudly on replay, the other
    succeeds quietly with the wrong logic.

**From the Phase 7 datastore inspection** — all measured against server 1.29.7 with
Elasticsearch 7.17.27; details and the queries that show them are in
[`docs/DATASTORES.md`](DATASTORES.md).

17. **Visibility indexes Runs, not customers**, so any list or count must exclude
    `ContinuedAsNew` or it double-counts anyone who has rolled over. Shipped as a bug in the
    Phase 4 list endpoint; see [§4](#4-search-attributes) for the numbers and the fix.
18. **`ORDER BY` is a limitation of Temporal's query language, not of Elasticsearch.** The same
    sort runs fine against `temporal_visibility_v1_dev` directly — it is only `ListWorkflow` /
    `temporal workflow list` that reject it. Sharpens item 8: the data *is* sortable, we simply
    cannot ask for it through the supported API, which is why the answer is filter-then-sort
    client-side rather than a read model.
19. **There is no `temporal_visibility` Postgres database when ES is the visibility store.**
    With `ENABLE_ES=true`, auto-setup creates only `temporal`. §8.1's earlier claim that the
    second database exists but stays empty describes a Postgres-visibility deployment, not this
    one.
20. **`visibility_tasks` is empty at rest** and only briefly non-empty under write, so a
    `SELECT count(*)` after the fact reports 0 even though the queue was used. Catching a row
    needs a poll running alongside the write — `make write-trace` does this.
21. **`_cat/indices` `docs.count` lags deletes.** After `make reap`, `_search` and `_count` drop
    to zero for the deleted runs while `docs.count` can stay high until merges clear
    soft-deletes. Use `_search`/`_count` to answer "is it gone?", not the index summary.
22. `history_node.tree_id` equals `executions.run_id` for these entity workflows, and the
    encoding is `Proto3` for `history_node.data`, `executions.data` and `executions.state`.
    Useful for anyone writing further SQL against persistence.

**From the Phase 5 history crawl** — measured against server 1.29.7.

25. **A reaped run's history comes back as `InvalidArgument`, not `NotFound`.** The message is
    *"Requested workflow history not found, may have passed retention period."*, and the same
    `InvalidArgument` type also covers a malformed run ID (*"Invalid RunId."*), so the type
    alone cannot decide it. Truncation detection is the one thing in this codebase that has to
    match on message text. Full table in [§6.3](#63-truncation-is-the-feature).

26. **`GetWorkflowHistory` with an empty run ID silently resolves to the latest run.** Helpful
    almost everywhere, wrong in a crawl: asking for a reaped predecessor with `""` returns the
    *newest* run's events rather than reporting the predecessor as gone, so the walk would loop
    on the same run instead of detecting truncation. Every run is addressed explicitly.

27. **The search attributes on a `WorkflowExecutionStarted` event belong to the previous run.**
    A run's own attributes are upserted on its first workflow task, so the started event of
    generation 2 carries `RewardsGeneration: 1`. The event's *input* payload is the carried
    state and is correct; the attributes on it are a snapshot of the run being left.

28. **The audit crawl is the most available read in the system, and the detail page the least.**
    Counterintuitive given the crawl is O(runs) and the detail page is one Query — but the
    crawl only reads persistence, while a Query needs a worker to replay history. With
    `make worker-stop`: `/audit` answers in 10 ms, `/api/customers/{id}` 503s. Taking
    `LifetimeEarnEvents` from the newest run's start payload rather than from a Query is what
    buys this, and it costs nothing.

**From the Phase 8 UI** — found building the React screens against both APIs.

29. **"Status" means two different things, in two different vocabularies.** The visibility
    query language says `ExecutionStatus = 'Running' | 'Canceled'`; the API's DTOs say
    `status: "active" | "deactivated"`. Same concept, same English word, no overlap in
    spelling — and they are not interchangeable in either direction, because one is a Temporal
    built-in search attribute and the other is our projection of it. A status filter in the UI
    has to translate. The mismatch is not accidental: a workflow cannot record its own closure
    ([§3.6](#36-deactivation-via-cancel)), so the API is deriving a rewards-domain word from a
    platform-domain one.

30. **The Go API sends no CORS headers; only the mock does.** So pointing a browser at it with
    a cross-origin base URL fails, and `make web` proxies `/api` through Vite instead
    (`VITE_API_PROXY_TARGET=http://localhost:8081`). The Phase 8 handoff brief claimed the two
    servers were interchangeable by base URL alone — they are field-for-field identical in
    *content*, which is what was actually verified, and that was written up as more than it
    was.

    Left as-is rather than "fixed" by adding permissive CORS: same-origin proxying is the
    normal Vite setup and the one that survives into production, whereas an unauthenticated API
    advertising `Access-Control-Allow-Origin: *` is a shape worth not copying out of a POC. The
    mock has it precisely because it is meant to be hit directly with no stack running.

**From the Phase 6 notification Activity.**

31. **A frozen contract cannot express a fact discovered after it was frozen.** The departure
    notice reuses the promotion Activity ([§3.7](#37-tier-promotion-notifications)), so both
    arrive at the audit crawl as a completed `NotifyCustomer` — but `AuditEntry`'s
    `notification_sent` kind carries a level and nothing else, and the UI renders every one of
    them as *"Promoted to Gold — notification sent"*. A customer who left therefore got an
    invented promotion rendered directly beneath their own `deactivated` row.

    Resolved by dropping the departure row rather than by adding an event field: the
    `deactivated` row above it already carries the fact, and the contract is frozen for the UI
    (§5.1). Worth naming as the cost of freezing — the freeze bought a UI built in parallel,
    and charged for it here.

32. **`EarnsPerRun` is a floor, not an exact count — and Phase 6 widened the gap.** The pre-roll
    guard holds the run open until the notifier is idle, and the Update handler keeps accepting
    adds the whole time, so a run that has already decided to roll can take on more. Measured
    on the real stack with six rapid adds:

    ```
    no tier crossing (6x50, stays basic)     adds per generation {0: 3, 1: 3}
    crosses gold on the 3rd (6x200)          adds per generation {0: 4, 1: 2}
    ```

    Reproducible, and isolated to the notification: the difference is exactly the time the
    promotion Activity holds the run open. Harmless — the extra add is applied, recorded and
    carried forward, and the roll still happens once — but "three adds per run" is an
    approximation under load rather than a rule.

    It is also a second argument for what [§3.5](#35-continue-as-new-after-3-updates) already
    says: a fixed count is the wrong trigger, `GetContinueAsNewSuggested` is what production
    should roll on, and a rule the system only approximately keeps is a poor thing to have
    written into a constant.

33. **An event-shaped condition cannot be retried; a state-shaped one can.** The promotion
    trigger started as *"did this add cross a tier boundary"*, which is an event: it happens
    once and is then gone. So a delivery that exhausted the Activity's (deliberately bounded)
    retries was lost for good — points only go up, so the boundary could never be crossed
    again, and nothing would ever re-offer it.

    Rewriting the condition as *"is the customer at a tier nobody has told them about"* makes
    it a property of the customer, so a later add retries it, and the state it reads is
    already carried across continue-as-new.

    **The retry is narrower than that sounds, and deliberately so.** It only re-offers the tier
    the customer is at *now*, so a failed gold notice is retried while they are still gold and
    dropped once they reach platinum. That is the right outcome — they get told where they are,
    and a belated "you reached gold" arriving after they are past it would be worse than
    silence — but the unqualified phrase "retried on the next add" over-claims it. Same class of
    over-claim as the comment that started this, so it is pinned by a test rather than trusted
    to a sentence. Same information, and the difference between a
    notification that can be recovered and one that cannot.

    It also made `NotifiedLevels` genuinely load-bearing. Under the crossing rule the monotonic
    balance did the deduplication and the field was belt-and-braces; now it is both the dedup
    and the retry ledger.

    Two consequences worth knowing. The queue has to reject a level it is already holding or
    delivering, or a provider that is down accumulates one entry per add and delays the roll
    without bound — reintroducing, one level up, exactly the failure that bounding the
    Activity's retries prevented. And the starting tier needs excluding explicitly: under the
    old rule `basic` could never be crossed into, and under this one it has to be said.

    Raised by review on PR #15.

34. **Background work that must outlive cancellation needs a disconnected context of its own.**
    The notifier goroutine runs on one, so that deactivating a customer mid-delivery does not
    cancel a promotion they had already earned. Running it on the workflow's own context looks
    natural and silently loses that notification — `handleLeave` then waits for a drain that
    can never happen, because the thing doing the draining died with the context.

**From the Phase 9 replay test.**

35. **`ReplayWorkflowHistory` substitutes a fake workflow ID, and the failure blames innocent
    code.** The replayer runs the workflow as `"ReplayId"` unless
    `ReplayWorkflowHistoryWithOptions` is given an `OriginalExecution`. Any workflow that reads
    its own ID therefore behaves differently under replay — ours rejects the enrollment payload
    (`validateEnrollment`, §3.1), returns before emitting a single command, and reports:

    ```
    nondeterministic workflow: missing replay command for UpsertWorkflowSearchAttributes
    ```

    Which names a line that is entirely innocent, and says "nondeterministic", so it reads as a
    versioning problem rather than a harness one. The option's doc comment calls it "Optional".
    It is optional only for workflows that never look at their own identity.

36. **`GetVersion` writes two events, not one**: the `Version` marker *and* an automatic upsert
    of the built-in `TemporalChangeVersion` search attribute. The second one is a gift —
    executions become queryable by which version of the code they are running, which is exactly
    what you want during a migration:

    ```
    WorkflowType = 'CustomerRewardsWorkflow' AND TemporalChangeVersion = 'tier-notifications-1'
    ```

    That answers "how many customers have picked up the change yet?" from the Temporal UI, with
    no instrumentation of our own.

**Design**

37. Because Updates are serialized by the workflow, concurrent point-adds cannot lose an
    update — no optimistic locking, no transactions, no retry loop. This is a genuine
    advantage over the obvious Postgres implementation and deserves a callout in the README.
38. Points spending / expiry, tier downgrade over time, and tier-anniversary review are all
    out of scope — and spending is now explicitly *decided against*, not merely deferred
    ([§3.1](#31-state-carried-across-continue-as-new)). The entity workflow with a durable
    timer is exactly where they'd go. Worth one paragraph as "what this shape buys you next."

---

## 13. Explicitly out of scope

Auth, multi-tenancy, points spending, tier expiry, archival, Codec Server, Worker Versioning,
production deployment, pagination and caching of the audit crawl, and horizontal scaling of
workers.
