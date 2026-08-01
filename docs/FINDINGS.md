# Findings

What building this POC established about Temporal, measured rather than assumed.

Everything here was verified against **Temporal server 1.29.7**, **Elasticsearch 7.17.27**
and the **Go SDK**, on the stack in `deploy/`. Where a number appears it was observed, not
estimated. Where a prediction turned out wrong, both the prediction and the correction are
kept — the corrections are the most useful thing in this repo.

The code cites these findings by anchor (`docs/FINDINGS.md#truncation-detection`). Headings
are therefore load-bearing: renaming one breaks the references pointing at it. An earlier
numbered scheme drifted — items were deleted without renumbering, leaving comments citing
`12.13` when they meant `12.10` — which is why the anchors are descriptive.

For how Temporal uses Postgres and Elasticsearch underneath, see
[DATASTORES.md](DATASTORES.md). For running the thing, see the [README](../README.md).

---

## Contents

- [Workflow design](#workflow-design) — the integrity boundary, tiers, the validator/handler
  split, continue-as-new, notifications, soft deactivation
- [Search attributes and visibility](#search-attributes-and-visibility) — the registered set,
  the registration cache, runs-vs-customers, ORDER BY, prefix search, visibility lag
- [The HTTP API](#the-http-api) — error classification, timeouts, the rollover race,
  pagination
- [The history crawl](#the-history-crawl) — walking the run chain, events read, truncation
- [Versioning and replay](#versioning-and-replay) — the deploy that would have wedged every
  customer, replay harness traps, stale workers
- [The local stack](#the-local-stack) — retention floor, Elasticsearch tuning
- [Smaller edges](#smaller-edges)
- [Out of scope](#out-of-scope)

---

## Workflow design

One long-lived Entity Workflow per customer. Workflow ID `customer-<customerID>`, task queue
`rewards`, type `CustomerRewardsWorkflow`. The ID gives natural dedup and a stable handle for
every later operation, so no lookup table is needed.

**All rewards state transitions are pure functions of workflow state** — no Activity is needed
to compute a balance or a tier. That is the "Temporal as a data store" argument. The single
Activity that does exist ([NotifyCustomer](#tier-promotion-notifications)) is there because
talking to the outside world is the one thing a workflow cannot do itself, which marks where
the boundary actually sits.

### The workflow is the integrity boundary

The cost of the headline decision. With no application database there is no schema, no `CHECK`
constraint, no `NOT NULL`, and no unique index standing behind this state. A workflow accepts
whatever payload it is started with, so **every invariant a table definition would have
enforced has to be written by hand**, at the top of the workflow, or it is not enforced at all.

`validateEnrollment` checks on start:

- `CustomerID` must match the workflow ID it was started under — otherwise search attributes
  and `getStatus` report one customer while every operation is keyed by another.
- Counters must be non-negative.
- `Points` must not already exceed the cap.
- Points cannot exist without an earn event to have earned them in.

A rejected enrollment fails the execution outright (`WorkflowExecutionFailed`, attempt 1, no
retry loop — verified) rather than producing a running customer whose numbers do not add up.

#### Points only go up

There is no spending, redemption, expiry, or manual adjustment, and none is planned.
`addPoints` is the only thing that ever writes `Points`, and it only ever adds. Balances are
monotonic for the life of a customer.

The consequence is that there is no separate lifetime total. An earlier draft carried both a
current `Points` and a `LifetimePoints`, on the theory that spending would eventually make them
diverge. With a monotonic balance they are the same number, always — so carrying both bought
nothing except an invariant to violate. It duly got violated: because the handler's cap was
measured against the caller-supplied `LifetimePoints`, a start payload with a large `Points`
and a zero `LifetimePoints` walked straight past it. One field, one number, no gap to exploit.

`LifetimeEarnEvents` is *not* redundant in the same way and stays: it counts adds, not points,
and [truncation detection](#truncation-detection) needs it to quantify how much of the audit
log has been reaped.

Two things follow, both places a reviewer will reasonably expect different behaviour:

- **Product leave does not reset the balance.** [Soft deactivation](#soft-deactivation) keeps
  the execution Running with `Deactivated` set; re-enrollment clears the flag and restores the
  same points. The only way a balance ever decreases is an *ops* cancel that closes the
  execution, after which a fresh Start is a new enrollment at zero.
- **Tier demotion cannot happen** through normal operation. Since tiers are derived from a
  monotonic balance, a customer's tier is monotonic too. The only way to demote is to raise a
  threshold, which retroactively demotes everyone at once.

If spending ever arrives, it means reintroducing a separate lifetime field — not repurposing
`Points`. The cap, the tier derivation, and the audit log would all need to pick a side.

#### State carried across continue-as-new

```go
type CustomerState struct {
    CustomerID string
    Name       string
    Email      string

    Points     int // monotonic -- only ever increases

    // Survives history reaping -- this is what lets the audit log detect and
    // quantify its own truncation.
    EnrolledAt         time.Time // original enrollment, from the very first run
    LifetimeEarnEvents int       // count of all successful adds, ever
    Generation         int       // how many times we've continued-as-new

    // Levels we've already sent a promotion notification for. Guards against
    // at-least-once Activity delivery re-notifying after a replay.
    NotifiedLevels []string

    // Soft leave. Zero value (false) means active -- including continue-as-new
    // payloads from before this field existed.
    Deactivated bool
}
```

### Tiers are derived, never stored

The thresholds live in one ordered table, and everything tier-shaped walks it:

```go
// when points >= GoldThreshold:     gold
// when points >= PlatinumThreshold: platinum
var tiers = []Tier{
    {Level: LevelGold, MinPoints: GoldThreshold},
    {Level: LevelPlatinum, MinPoints: PlatinumThreshold},
}

func Ladder() []Tier // a copy of the rungs, for callers outside the package

func Level(points int) string          // the highest rung reached
func NextTierAt(points int) (int, bool) // the first rung not reached
func promotionFor(state *CustomerState) // the highest rung not yet announced
```

Each of those three was its own `switch` over the same two constants until the ladder replaced
them. That is three places to edit when a tier is added and two to forget, and both failures
are quiet: a missing case in `NextTierAt` is a wrong progress bar, and a missing case in
`promotionFor` is a promotion nobody is told about. Adding a tier is now one line, guarded by
`TestTierLadderIsOrdered` — the ordering is load-bearing for all three readers.

`basic` is deliberately not a rung. It is the floor, what you are when no rule matches, so
`promotionFor` needs no clause to avoid congratulating anyone for reaching it.

**There was a fourth reader, and it was in the browser.** `CustomerResponse` carried
`nextTierAt` and nothing else, which is enough to *name* the next target but not to draw a bar:
that also needs the rung the customer is standing on. The UI reverse-looked-it-up from the one
number it had —

```tsx
const prev = nextTierAt === 500 ? 0 : nextTierAt === 1000 ? 500 : 0
```

— so "adding a tier is one line" was false, and the missed line was in another language, in
another build artifact, with no test in front of it. It fails the quietest way yet: a bar of the
wrong width renders perfectly. The response now carries the whole ladder (`tiers`, ascending,
`basic` absent) and the client derives its floor from that. The ladder comes back from the
**Query** rather than being attached by the API, because `api` and `worker` are separate
binaries on separate deploys — an API pairing its own rungs with a `nextTierAt` from another
build could name a target that is not on the ladder printed beside it. Which also means a
`make api` without a `make worker` serves `tiers: null` until the worker catches up; the bar
degrades to spanning the whole climb rather than the current segment.

**Which direction `promotionFor` walks the ladder is the "only the tier they are at" decision**
([notifications](#tier-promotion-notifications)), and it is where you would go to decide
otherwise. Top-down and return announces where the customer is. Announcing every tier they
passed takes *two* changes, and the first one alone is a trap worth recording: reversing the
walk by itself announces the **lowest** unannounced tier, so one 1000-point add lands a
customer in platinum and congratulates them for gold. `deliverPromotion` sends one notification
per drain, so the other policy needs the reversed walk *and* a `deliverPromotion` that loops
until `promotionFor` returns false.

Measured, not reasoned: the flip alone yields `[gold]`, the flip plus the loop yields
`[gold platinum]`, and both fail exactly `Test_Notify_SingleAddPastTwoTiersAnnouncesOnlyTheNewOne`
and `Test_Notify_RetryDoesNotSurviveAdvancingATier` and nothing else — so the decision cannot
be reversed by accident.

Deriving rather than storing means a threshold change applies uniformly and replays
identically. The trade-off is that lowering a threshold retroactively promotes everyone and
raising it retroactively demotes them. A real program would want an explicitly stored,
monotonic "achieved tier."

### Handlers

| Kind | Name | Signature |
|---|---|---|
| Update | `addPoints` | `AddPointsRequest{Amount int, Reason string} → AddPointsResult{Balance int, Level string, EventID string}` |
| Update | `deactivate` | `→ DeactivateResult{Changed bool}` — soft leave; idempotent |
| Update | `reactivate` | `ReactivateRequest{Name, Email} → ReactivateResult{Changed bool, Status CustomerStatus}` — restore membership |
| Query | `getStatus` | `→ CustomerStatus{…, Active bool, …}` — `Active` is `!Deactivated` |

### The validator/handler split

Requirement: points must be positive and must not exceed 1000 per transaction. *Where* that is
enforced is the interesting part.

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
row holding both the ask and the answer — see [events the crawl reads](#events-the-crawl-reads).

#### The decision this forces

"Show the history of when points were added" is satisfied on the happy path no matter where we
validate, because successful adds are always recorded. What it doesn't answer is whether a
**rejected attempt** belongs in the audit log — and for a rewards program the honest answer
differs by reason:

- *"Amount was −50"* / *"amount was 6000"* — a client bug or a fat-finger. Nobody reviewing a
  customer's account cares. Recording it is noise.
- *"This add would push them past their points cap"* — genuinely useful. A support rep asking
  "why didn't this customer reach platinum?" wants exactly that row.

So both phases are used for what each is good at:

- **Validator** (leaves no history) — `Amount <= 0`, `Amount > MaxPointsPerTxn`, empty reason.
- **Handler returning an error** (both events recorded) — two business rules: reject if the add
  would push `Points` past `PointsCap`, and reject adds to a soft-deactivated customer. Both
  depend on accumulated customer state, which is what earns them a place in history.

#### Why this is more than a stylistic choice

Two independent consequences push the same way.

**History bloat and abuse.** We continue-as-new every 3 adds, and history has hard limits
(50k events / 50 MB). Validator rejections are *free* — a buggy retry loop hammering
`amount: -1` cannot grow history at all. Handler rejections are recorded, so every rule
enforced there is a surface for history growth. Keep that set small and meaningful.

**Determinism surface.** Handler code is replayed; validator rejections leave nothing to
replay. If the points cap lives in the handler and the constant later changes, we have changed
workflow code that must still reproduce already-recorded outcomes — an update recorded as
rejected under the old threshold could evaluate differently on replay. Rules in the validator
carry no such risk, because there is no recorded decision to contradict. This is a real
constraint on how freely the handler-side rules can be tuned later, and it is worth verifying
with the [replay test](#versioning-is-the-real-risk).

Rule of thumb: **the validator is for facts about the request; the handler is for facts about
the customer.** Input shape, ranges, and required fields go in the validator. Anything that
depends on accumulated workflow state and that a human would want to see later goes in the
handler, accepting that it becomes part of the versioning surface.

#### Alternatives, if we change our minds

Both are defensible and cheap to switch to:

- **Everything in the validator** — simplest, cleanest history, audit log shows only successful
  adds. Right if the audit log records what *happened*, not what was *attempted*.
- **Everything in the handler** — every attempt auditable, at the cost of history bloat and a
  larger determinism surface. Right if this were a financial ledger where attempted-and-denied
  is itself reportable.

The split is chosen partly because it lets the POC demonstrate both behaviours side by side:
trigger each rejection type and watch one appear in the Temporal UI's history while the other
leaves no trace.

### Continue-as-new

Two hard constraints from the platform:

1. **Continue-as-new is not supported inside an Update handler.** It must happen in the main
   workflow function.
2. **All handlers must be finished first**, or an in-flight Update is lost.

So the handler only mutates state and bumps a counter; the main function waits on it:

```go
if err := workflow.Await(ctx, func() bool {
    return earnsThisRun >= EarnsPerRun
}); err != nil {
    return err
}

// Let any concurrently-accepted update finish before we roll the run.
if err := workflow.Await(ctx, func() bool {
    return workflow.AllHandlersFinished(ctx)
}); err != nil {
    return err
}

state.Generation++
return workflow.NewContinueAsNewError(ctx, CustomerRewardsWorkflow, state)
```

#### A fixed count is the wrong rule, and we use it anyway

`EarnsPerRun = 3` is hardcoded, chosen because three adds is easy to demonstrate in a terminal
— not because it is defensible. What actually bounds a run is history **size**, and a count of
adds is only a proxy for it: three is wastefully early for small updates and would be far too
late if each add carried a large payload. The real limits are 50k events and 50 MB per run, and
neither is a number of adds.

The server already tracks the real thing:

```go
workflow.GetInfo(ctx).GetContinueAsNewSuggested()
```

which flips to true as a run approaches those limits, with `GetContinueAsNewSuggestedReasons()`
reporting which one. **Production should roll on that.**

An earlier version made the threshold configurable — env var, worker-level setter, zero meaning
"ask the server" — so both behaviours were demonstrable. That was removed. It bought a demo of
the correct behaviour at the cost of a mutable knob on a value that is baked into recorded
history, which is precisely the shape that causes
[non-determinism](#versioning-is-the-real-risk). One hardcoded constant with a comment
explaining what production should do instead is both simpler and harder to misuse than a switch
inviting people to change it at runtime.

#### EarnsPerRun is a floor, not an exact count

The run delivers any pending promotion before rolling, and the Update handler keeps accepting
adds for the duration of that Activity, so a run that has already decided to roll can take on
more. Measured on the real stack with six rapid adds:

```
no tier crossing (6x50, stays basic)     adds per generation {0: 3, 1: 3}
crosses gold on the 3rd (6x200)          adds per generation {0: 4, 1: 2}
```

Reproducible, and isolated to the notification: the difference is exactly the time the
promotion Activity holds the run open. It survived the rewrite from a drain goroutine to
main-loop delivery unchanged, which is what you would expect — the cause is the Activity's
round trip, not the structure around it.

Harmless: the extra add is applied, recorded and carried forward, and the roll still happens
once. But "three adds per run" is an approximation under load rather than a rule — and a second
argument that a fixed count is the wrong trigger.

### Soft deactivation

Product leave is an Update, not a cancellation. `deactivate` sets
`CustomerState.Deactivated = true`, upserts `RewardsActive = false`, arms the departure
notification, and returns. The execution stays `Running`. `reactivate` clears the flag,
optionally refreshes name/email, upserts `RewardsActive = true`, and returns the same balance
the customer left with. The audit timeline shows a `deactivated` row followed by a
`reactivated` one — there is no closed run in between.

That is a deliberate reversal of an earlier cancel-based design. Cancel closed the execution,
freed the workflow ID, and made re-enrollment a fresh Start at zero — irreversible in a way
"deactivate" does not imply, and dependent on history that `make reap` usually deletes. Soft
leave keeps membership as workflow state, so restore does not need the old run's history at
all. **Membership for the list is `RewardsActive`, not `ExecutionStatus`**: a soft-left customer
is still `Running`.

`DELETE /api/customers/{id}` is the deactivate Update (idempotent: already-deactivated →
`Changed: false`). `POST /api/customers` against a soft-deactivated ID reactivates rather than
409ing; against an *active* running customer it still 409s.

**Out-of-band cancel is the ops path, not the product one.** `temporal workflow cancel` (or
Terminate) still closes the execution. Re-enrolling that ID is a fresh Start at zero — the old
balance left with the closed run. `make reset` is the clean slate for demos.

#### A duplicate start is silent by default

A double-create against a *running active* customer should return a clean 409 rather than
silently attaching — which takes **two** settings on `StartWorkflowOptions`, not one:

```go
WorkflowIDConflictPolicy:                 enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
WorkflowExecutionErrorWhenAlreadyStarted: true, // without this, ExecuteWorkflow returns nil
```

With the conflict policy alone, `client.ExecuteWorkflow` returns the *existing* `WorkflowRun`
and a **nil error**, so there is nothing for the API to map to a 409. Only with the second flag
does it return `serviceerror.WorkflowExecutionAlreadyStarted`. The conflict policy governs what
the server does; this flag governs whether the SDK tells you about it. (`WorkflowIDConflictPolicy`
also already defaults to `Fail`, so the flag that looks redundant is the load-bearing one.)

**The `temporal` CLI hides this too**: `workflow start --id-conflict-policy Fail` against a
running execution prints `Running execution:` with the existing run ID and **exits 0**. So the
CLI cannot be used to verify this behaviour — check it through the SDK.

### Tier promotion notifications

The one Activity in the system. `NotifyCustomer(ctx, NotifyRequest{CustomerID, Email, Event,
Level})` logs a line saying what would be sent, and returns. The body is a stub — production
would call an email or push service here — but everything *around* it is real.

Triggered when a point-add leaves the customer at a tier they have not been told about. Because
tiers are derived this is a pure comparison inside the handler: is `Level(points)` absent from
`NotifiedLevels`?

**One notification per customer per tier reached, naming where they are now — not one per
threshold passed.** `MaxPointsPerTxn` is 1000 and platinum starts at 1000, so a single add from
zero lands a customer in platinum having never been observed at gold, and only platinum is
announced. Sending "Welcome to Gold" beside "Welcome to Platinum" would describe a state that
held for no measurable time.

#### Where it runs, and why not in the handler

The tempting thing is to await the Activity inside the `addPoints` handler, so the Update does
not return until the customer has been notified. Don't:

- **It couples two operations with different failure semantics.** The points were legitimately
  earned and are already recorded in history. A notification service being down must not fail
  the point-add or roll it back — but an awaited Activity error inside the handler does exactly
  that.
- **It puts a network call on the UI's critical path.** With a default retry policy an
  unreachable notifier would retry indefinitely, so the Update hangs until the client times out
  — with the points still awarded. The worst possible UX for the clearest possible reason.

So the handler stays synchronous and cheap: it applies the points, notices that the customer
sits at an unannounced tier, arms a flag, and returns the new balance immediately. **The main
loop** observes that flag and runs the Activity:

```go
for {
    workflow.Await(ctx, func() bool {
        return needsNotify || needsDeparture || earnsThisRun >= EarnsPerRun
    })

    if needsNotify {                                  // promotions first
        needsNotify = false
        deliverPromotion(ctx, &state)
        continue
    }

    if needsDeparture {                               // ...then the departure notice
        needsDeparture = false
        sendNotify(ctx, departureNotice(&state))
        continue
    }
    ...                                               // ...then the roll
}
```

Notify → depart → continue-as-new, in that order, with the loop re-entered after each step so
the ordering holds however many things arrive at once. A pending promotion is always drained
before the departure notice, and both before a roll.

**The Activity's retries must be bounded.** The default policy retries forever, and delivery
happens inline in the main loop, so an unreachable notification provider would hold the loop
itself — and with it the roll — for as long as it stayed down. A cosmetic outage would become a
stuck entity workflow. `MaximumAttempts: 3`.

#### At-least-once, and what that means here

Activities are at-least-once: a worker crash after `NotifyCustomer` runs but before its
completion is recorded means it runs again on replay. For a real email that is a duplicate in
someone's inbox.

Two mitigations, both cheap:

- Carry `NotifiedLevels []string` in `CustomerState` (so it survives continue-as-new) and skip
  any level already in it. This is the workflow-side guard, and it is also the retry ledger: a
  level stays out of it until a delivery actually succeeds, so a failed one is re-offered by the
  next add.
- Pass an idempotency key of `<customerID>:<level>` to the Activity, since a customer reaches
  gold exactly once. This is the guard a real notification service would honour, and including
  it in the stub documents the contract even though the stub ignores it.

#### An event-shaped condition cannot be retried; a state-shaped one can

The promotion trigger started as *"did this add cross a tier boundary"*, which is an event: it
happens once and is then gone. So a delivery that exhausted the Activity's (deliberately
bounded) retries was lost for good — points only go up, so the boundary could never be crossed
again, and nothing would ever re-offer it.

Rewriting the condition as *"is the customer at a tier nobody has told them about"* makes it a
property of the customer, so a later add retries it, and the state it reads is already carried
across continue-as-new.

**The retry is narrower than that sounds, and deliberately so.** It only re-offers the tier the
customer is at *now*, so a failed gold notice is retried while they are still gold and dropped
once they reach platinum. That is the right outcome — they get told where they are, and a
belated "you reached gold" arriving after they are past it would be worse than silence — but
the unqualified phrase "retried on the next add" over-claims it, so it is pinned by a test
rather than trusted to a sentence.

It also made `NotifiedLevels` genuinely load-bearing. Under the crossing rule the monotonic
balance did the deduplication and the field was belt-and-braces; now it is both the dedup and
the retry ledger.

Two consequences worth knowing. The queue has to reject a level it is already holding or
delivering, or a provider that is down accumulates one entry per add and delays the roll
without bound — reintroducing, one level up, exactly the failure that bounding the Activity's
retries prevented. And the starting tier needs excluding explicitly: under the old rule `basic`
could never be crossed into, and under this one it has to be said.

#### Reuse for departure

Soft deactivate arms a departure notice; the main loop delivers it with `Event: "departed"` on
the workflow's own context — the same Activity as promotions, no separate cleanup Activity. An
out-of-band cancel skips that path entirely.

Delivery runs on the workflow's own context, and it used to run on a disconnected one. While
leaving was a cancellation, delivering on `ctx` meant a deactivation arriving mid-delivery
cancelled a promotion the customer had already earned, so the loop wrapped it in
`workflow.NewDisconnectedContext`. Soft deactivation removed the premise: nothing cancels the
workflow, so there is no cancellation for the delivery to outlive, and the wrapper went with
it. Ordering carries the guarantee instead — the loop drains any armed promotion *before* the
departure notice, so a customer who reached gold and then left still hears about gold.

---

## Search attributes and visibility

Registered once at bootstrap, before any workflow starts, using the typed API
(`temporal.NewSearchAttributeKey*` / `workflow.UpsertTypedSearchAttributes`).
`deploy/bootstrap.sh` and `internal/rewards/searchattr.go` must both match this table.

| Name | Type | Written |
|---|---|---|
| `CustomerId` | Keyword | at start, re-asserted each run |
| `CustomerEmail` | Keyword | at start |
| `CustomerName` | Text | at start (tokenized → word-prefix search) |
| `RewardsLevel` | Keyword | on every balance change |
| `RewardsPoints` | Int | on every balance change |
| `RewardsEnrolledAt` | Datetime | at start of each run, from carried state |
| `RewardsGeneration` | Int | on continue-as-new |
| `RewardsActive` | Bool | at start `true`; flipped by deactivate / reactivate |

Built-ins used: `ExecutionStatus`, `StartTime`, `CloseTime`, `WorkflowId`, `RunId`.

Two notes:

- Search attributes should be inherited by the continued-as-new run, but re-upsert them at the
  top of every run anyway. It is cheap, and it means one code path establishes the invariant
  rather than relying on inheritance behaviour we would otherwise have to verify on every SDK
  upgrade.
- Registration must be idempotent — `bootstrap.sh` tolerates "already exists" so `make up` is
  safe to re-run. Forgetting the bootstrap step produces an empty customer list with no error,
  which is a confusing first-run experience; it is wired into `make up` rather than documented
  as a manual step.

### Registering a search attribute does not make it usable

**A newly registered search attribute is invisible to workflow tasks for about a minute.** The
server caches the definitions, and `UpsertSearchAttributes` is validated against that cache, so
for the first ~60s after `bootstrap.sh` registers them the workflow task fails with:

```
BadSearchAttributes: search attribute CustomerId is not defined
```

Which surfaces at the caller as a *successful* enrollment followed by every Update failing with
`Unable to perform workflow execution update due to unexpected workflow task failure` — a 500,
not a 400, and nothing in it names search attributes. Starting a workflow is unaffected, because
the upsert happens inside the workflow: it looks like adding points is broken, when what is
actually broken is the attribute registry the caller never touched.

Measured on a cold stack: 16 of 18 seeded customers failed their first add, and the same code
succeeded ~33 s later with no change. The workflow tasks were retried and eventually applied, so
the data arrived — after the seed had already reported failures and exited non-zero.

Nothing on the client side fixes this; it is server state. `system.forceSearchAttributesCacheRefreshOnRead`
reads through the cache instead, at the cost of a lookup per read, and is documented as a
development-only setting — which is exactly the tradeoff a stack that bootstraps and seeds in the
same command wants. It is in `deploy/dynamicconfig/dev.yaml`.

The general shape is worth remembering for a real deployment, where the same window exists but is
usually hidden by the gap between provisioning a namespace and deploying code against it:
**register search attributes well before the first workflow that upserts them.**

### Visibility indexes runs, not customers

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

That leaves exactly the current generation whatever its final state — `Running` for both active
and soft-deactivated customers (tell them apart with `RewardsActive`), `Canceled` / `Terminated`
for an ops-closed run, `Failed` for an enrollment that never validated.
`IN ('Running','Canceled')` looks equivalent and silently drops that last group: 45 against 47
on the same data.

This shipped as a bug in the list endpoint and was caught by the datastore inspection — which
is the argument for [DATASTORES.md](DATASTORES.md) being a deliverable rather than a
nice-to-have. No API test would have found it, because the API was faithfully reporting what it
was asked for.

This powers the customer list page's **filtering**:
`RewardsLevel = "gold" AND ExecutionStatus = "Running" AND RewardsActive = true`.

### ORDER BY is not supported

`ORDER BY` is rejected outright by server 1.29.7 — `ORDER BY clause is not supported` — and not
just for custom attributes: it is refused for built-ins like `StartTime` and `CloseTime` too,
with Elasticsearch visibility active and custom search attributes otherwise working normally.
Verified against the real stack, so this is the platform's answer, not a misconfiguration.

**The limitation is Temporal's query language, not Elasticsearch.** The same sort runs fine
against `temporal_visibility_v1_dev` directly (`make inspect-es Q=gold-running`); it is only
`ListWorkflow` / `temporal workflow list` that reject it. The data *is* sortable, we simply
cannot ask for it through the supported API.

What still works, and is enough: equality and range filters on custom attributes
(`RewardsPoints >= 500`), word-prefix search on `CustomerName` (below), `Keyword` exact match on
`CustomerEmail`, `RewardsActive` for active-vs-deactivated, and `ExecutionStatus` to exclude
rolled-over generations. Results come back in the server's default order (most recent first).

The consequence: **sorting must happen client-side**, which is only equivalent to server-side
sorting when the full filtered result set fits in one page. Sorting a page of an arbitrarily
paginated list sorts the wrong thing. For a POC with tens of customers, fetch the filtered set
and sort in the API. The honest answer is that Temporal's visibility store is a filter index,
not a reporting database.

### Prefix search works on a Text attribute, with three catches

`STARTS_WITH` is documented for `Keyword`, but server 1.29.7 accepts it on `Text` too, and there
it does what a search box wants: a prefix match against each *token*, so `lovel` finds
Ada Lovelace. That is what the customer list's name box builds, one clause per word typed —
which also fixes an OR that surprised us. All verified against the real stack:

| Query | Matches |
|---|---|
| `CustomerName = 'lovelace'` | Ada Lovelace |
| `CustomerName = 'lovel'` | — whole tokens only |
| `CustomerName = 'ada turing'` | Ada Lovelace **and** Alan Turing — `=` ORs its words |
| `CustomerName STARTS_WITH 'lovel'` | Ada Lovelace |
| `CustomerName STARTS_WITH 'ada lov'` | — the literal is one prefix, not two |
| `… STARTS_WITH 'ada' AND … STARTS_WITH 'lov'` | Ada Lovelace |

The catches, all of them consequences of `STARTS_WITH` matching the stored token directly
rather than being run through the analyzer the way `=` is:

- **It is case-sensitive.** The standard analyzer lowercased the tokens at index time and
  nothing lowercases the prefix, so `Lovel` matches nothing. Lowercase before you send it.
- **The literal is a single prefix.** `'ada lov'` is looked up as one token beginning with
  "ada lov", and tokens have no spaces. Split the input and AND one clause per word — which is
  also the behaviour you want, since it narrows as the user types where `=` widens.
- **Split the input the way the analyzer did.** The standard tokenizer breaks on punctuation
  but keeps intra-word apostrophes: `Mary-Jane` is two tokens, `O'Brien` is one. Splitting on
  whitespace alone leaves `mary-jane` matching nothing.

One thing that does not work at all: **an apostrophe cannot be put into a query literal.**
Neither `\'` nor `''` round-trips — `CustomerName = 'o''brien'` and `CustomerName = 'o\'brien'`
both return zero against a document ES itself matches on `o'brien`. Prefix search happens to
have an out, since a shorter prefix is still a correct prefix: cut the term at the apostrophe
and search `o`, which matches more rather than nothing.

### Visibility lag

Elasticsearch visibility is asynchronous, so a newly created or updated customer does not appear
in `ListWorkflow` results immediately. Out of the box that gap is roughly **1–2 seconds**, which
is very visible in a UI. It has three components, and two of them are ours to tune:

| Component | Default | Tunable? |
|---|---|---|
| Visibility task processing (history service drains `visibility_tasks`) | single-digit ms | Not worth touching |
| Bulk processor buffering before the write is sent to ES | **1s** (`worker.ESProcessorFlushInterval`) | Yes — dynamic config |
| ES index refresh, after which the document becomes searchable | **1s** (ES default for `index.refresh_interval`) | Yes — index setting |

The second is a Temporal dynamic config setting; the default was raised from 200 ms to 1 s in
server v1.12 for throughput reasons that do not apply here. In `dynamicconfig/dev.yaml`:

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

**But not to true zero, and it can't be.** ES is a near-real-time search engine; the refresh is
*what makes* a document searchable, and Temporal's bulk processor exposes no `refresh=wait_for`
option to force it per write. Driving `refresh_interval` toward zero just trades latency for
constant segment churn.

So the honest answer has two halves. Tune the above, *and* don't route read-after-write through
visibility in the first place: `QueryWorkflow` and `DescribeWorkflowExecution` read from
persistence rather than the visibility store, so they are strongly consistent and have no lag at
all. That is why the create flow redirects to the detail page — it is not a workaround for slow
indexing, it is reading from the store that actually has the answer. Visibility is only needed
for the list page, which is inherently a "roughly now" view.

The lag is not a black box: the `visibility_tasks` table in Postgres is the queue feeding this
pipeline, so the delay can be watched draining rather than taken on faith. See
[DATASTORES.md](DATASTORES.md).

---

## The HTTP API

```
POST   /api/customers              → ExecuteWorkflow, or reactivate if soft-deactivated
GET    /api/customers?q=<sql>      → ListWorkflow + CountWorkflow
GET    /api/customers/{id}         → QueryWorkflow(getStatus) + Describe
POST   /api/customers/{id}/points  → UpdateWorkflow(addPoints), synchronous
DELETE /api/customers/{id}         → UpdateWorkflow(deactivate), soft leave
GET    /api/customers/{id}/audit   → history crawl
```

The API holds a Temporal Client and nothing else — no database, no cache, no ORM. That is the
whole argument of the POC and the code should make it obvious at a glance.

### Error classification

The failure modes a reviewer will poke at, and how each is actually detected:

| Condition | HTTP | How it is detected |
|---|---|---|
| Malformed or incomplete request | 400 | validated in the handler, before Temporal |
| Update rejected by validator or handler | 422 + message | `*temporal.ApplicationError` — see below |
| Add to a soft-deactivated customer | 409 + `deactivated` | `*temporal.ApplicationError` with type `Deactivated` |
| Customer already exists and is running | 409 | `*serviceerror.WorkflowExecutionAlreadyStarted` |
| Workflow not found / history reaped | 404 | `*serviceerror.NotFound` |
| No worker polling | 503 + "worker unavailable" | `FailedPrecondition`, `DeadlineExceeded`, or our own timeout |
| Update lost to a continue-as-new race | retried once, then 409 | best-effort; see [the rollover race](#the-rollover-race) |

**Both halves of the [validator/handler split](#the-validatorhandler-split) arrive as
`*temporal.ApplicationError`**, and are told apart only by `Type()`: a handler rejection carries
the type we chose (`PointsCapExceeded`), a validator rejection carries an empty one. That is
more convenient than expected — no message matching is needed to separate a business rejection
from an outage, which was the thing most likely to go wrong here. Both map to 422 carrying
`appErr.Message()`, which excludes the SDK's `(type: ..., retryable: ...)` suffix. The caller
cannot tell the two apart, which is the intent of the split, not a limitation.

The third handler rejection, type `Deactivated`, is the exception: it maps to 409 with code
`deactivated`, because it reports membership state rather than a problem with the add.

### Read and write timeouts

Timeouts are load-bearing on both read paths, and were sized from measurement rather than taste.

**No worker polling fails differently depending on which API you call, and on how long the
worker has been gone.**

- A **Query** with no worker takes ~9–10s to fail, and *how* it fails is not stable: within a
  few seconds of the worker dying it is a bare gRPC `RST_STREAM` (a 500), later it is
  `FailedPrecondition` or `DeadlineExceeded`. A **2 s** bound means our own deadline usually
  wins the race and all three collapse into one predictable 503 — against ~30 ms for a healthy
  query, so the headroom is ~60×.
- An **Update** with no worker does not fail at all. It *blocks* — observed still waiting after
  two minutes. Without a bound, `POST /points` hangs for as long as the client will hold the
  connection whenever the worker is down. **15 s**.

The asymmetry is worth internalising: the same underlying condition fails fast on one API and
never on the other. During development the worker will be down often, so this is the failure
mode a reviewer is most likely to meet first.

### The rollover race

An Update in flight when a run rolls over gets aborted; the client must retry against the new
run. With `EarnsPerRun = 3` this race is *frequent* — every third add can hit it. The API must
retry transparently or the demo looks broken.

**The error that reports it is ambiguous, and getting this wrong is easy.** An Update whose run
has closed comes back as `*serviceerror.NotFound` carrying *"workflow execution already
completed"* — and that is the same error, byte for byte, whether the run closed because it
continued-as-new or because the customer deactivated. It has to be: in both cases the run really
did complete. The two need opposite responses (retry vs refuse), so **the error alone cannot
decide**. Ask the server what is running now: a successor execution means a rollover, no open
execution means a departed customer. Matching the message instead shipped "please retry" to
callers adding points to a deactivated customer, where retrying can never succeed.

**And "operate on a closed execution" is not one behaviour — it depends on the operation.**
Measured on 1.29.7:

| on a closed execution | `Canceled` | `Failed` | `Terminated` | never existed |
|---|---|---|---|---|
| `CancelWorkflow` | `nil` | `nil` | `nil` | `NotFound` |
| `UpdateWorkflow` | `NotFound` | — | — | `NotFound` |

Cancel is idempotent server-side; Update is not. Assuming the two behave alike is a reasonable
guess and a wrong one.

That asymmetry stopped protecting `DELETE` when it stopped being a cancel. It is the
`deactivate` Update now, so it needs the same rollover disambiguation `POST /points` does, and
its idempotency comes from the handler rather than the server: a repeat returns
`Changed: false`, which is also what keeps a second DELETE from drawing a second row on the
timeline.

**The update-side rollover retry has never been observed firing.** Sequential adds complete
before the next begins, so the roll finishes in the gap and no update is ever in flight across
it. The retry is correct *if* it fires — it disambiguates via Describe rather than by message —
but "correct and unexercised" is what it is. A UI issuing overlapping adds from a browser is the
first thing likely to exercise it.

### Queries race continue-as-new too

The rollover race anticipates an *Update* being aborted. A **Query** immediately after a
point-add that triggered one can also fail, with `Workflow task is not scheduled yet.` — the
successor run exists but has no workflow task to dispatch the query to.

Observed once through the API at three adds per run, transient, and **not reproducible on demand
despite deliberate attempts**. The API retries unrecognised query failures once rather than
matching that message, on the grounds that a ~30 ms idempotent read is cheap to repeat and the
error could not be pinned down. Recorded because the *class* is real even though this instance is
not reliably reproducible, and because "not scheduled yet" will look like a bug to whoever meets
it next.

### Update dedup does not survive continue-as-new

Update dedup via `UpdateID` is scoped to a single run, so a retry that straddles a rollover can
double-apply. The UI sends a UUID per click. Per-run dedup is adequate for points and **not
sufficient for real money**.

### No pagination, and a frozen contract

`CustomerListItem`, `CustomerListResponse`, `AuditEntry` and `AuditResponse` were defined in
`internal/httpapi/dto.go` **before** the endpoints that return them, so the UI could be built in
parallel rather than serialised behind them.

Two shape decisions encode a platform constraint rather than a preference:

- **`CustomerListItem` is deliberately narrower than `CustomerResponse`.** The list is served by
  `ListWorkflow`, which returns *search attributes only*, so `LifetimeEarnEvents` is absent by
  construction rather than by omission.
- **There is no pagination.** The list returns at most `ListLimit` (5) customers, reports how
  many matched in total, and tells the user to filter. This follows from
  [ORDER BY being rejected](#order-by-is-not-supported): with no stable ordering, "page 2 of
  customers" does not mean anything in particular, so paging would be machinery that cannot be
  made coherent.

  The total comes from `CountWorkflow`, a separate API from `ListWorkflow` that — verified
  against 1.29.7 — honours the same filters:

  ```
  WorkflowType = 'CustomerRewardsWorkflow'                                  count=58
  WorkflowType = 'CustomerRewardsWorkflow' AND ExecutionStatus = 'Running'  count=36
  RewardsLevel = 'gold'                                                     count=13
  ```

  So the notice is an exact "Showing 5 of 23 — filter to find additional results". `Total` is
  `-1` if the count call fails, which degrades the message to "of many" rather than failing the
  list. Being two queries, `Total` and `Items` can disagree by a row under concurrent writes;
  that is not worth solving for a number displayed next to the word "filter".

  Two things a caller must not assume: *which* five it gets is unspecified and can differ
  between identical requests, and sorting them is only sorting the real set when `Complete` is
  true — otherwise it is sorting a sample.

#### The cost of a frozen contract

**A frozen contract cannot express a fact discovered after it was frozen.** The departure notice
reuses the promotion Activity, so both arrive at the audit crawl as a completed `NotifyCustomer`
— but `AuditEntry`'s `notification_sent` kind carries a level and nothing else, and the UI
renders every one of them as *"Promoted to Gold — notification sent"*. A customer who left
therefore got an invented promotion rendered directly beneath their own `deactivated` row.

Resolved by dropping the departure row rather than by adding an event field: the `deactivated`
row above it already carries the fact. Worth naming as the cost of freezing — the freeze bought
a UI built in parallel, and charged for it here.

### Queries work on closed executions

**A soft-deactivated execution answers Queries perfectly well** — it is still Running, and the
Query returns full state including `Active: false`, balance, tier, and `LifetimeEarnEvents`. An
*ops-closed* execution (out-of-band cancel) also answers Queries via history replay, so the same
fields are available until the run is reaped.

Worth stating because assuming the opposite is easy and the cost is silent: the detail endpoint
initially short-circuited on status and fell straight to search attributes, which carry no
`LifetimeEarnEvents`, so departed customers read back missing a field that had been available
all along. The assumption was never tested because the code path that would have tested it was
skipped.

The search-attribute fallback survives, on the degraded path only: replay needs a worker, so a
closed customer with none would otherwise 503 despite the execution record already holding most
of the page. Falling back beats failing for someone who cannot change anyway.

It does **not** cover a reaped customer. `make reap` deletes the whole execution record, search
attributes included, so those fail at `Describe` and surface as a 404 rather than degrading.
Truncation detection is the crawl's job, not this endpoint's.

### Deactivation is a request, not a completion

Under the old cancel-based design, `DELETE /api/customers/{id}` returned as soon as the server
accepted the cancellation, while the execution stayed `Running` to drain the notifier and send
the departure notification. Measured: DELETE returned in 13 ms, the execution closed 75 ms later.
Anything that deactivated and then re-enrolled had to wait, because enrollment failed on
conflict and 60 ms was plenty of window.

**Both the race and the flag that met it are now moot**, which is worth recording rather than
deleting. Soft deactivation means the workflow never closes, so there is no window to race. The
finding survives because the property it describes does: **an operation that returns once a
*request* is accepted has not finished**, and code that reads state straight afterwards is racing
something.

Worth noticing *why* it became reliable rather than rare: putting an Activity in the departure
path turned a window measured in microseconds into one measured in tens of milliseconds. Adding
a network call to a shutdown path is a good way to promote a latent race into a certain one.

---

## The history crawl

The audit log is reconstructed by crawling raw Event History, which is the reason for the
3-update continue-as-new.

### Walking the run chain

A customer's life spans many Runs sharing one Workflow ID. Newest run first, walk backwards:

1. `DescribeWorkflowExecution(workflowID, "")` → the current run.
2. `GetWorkflowHistory(workflowID, runID)` → events for that run.
3. The first event, `WorkflowExecutionStarted`, carries `ContinuedExecutionRunId`. Non-empty
   means there is a previous run; recurse. Empty means this is the original enrollment run and
   the chain is complete.
4. Reverse to get oldest-first.

Two details, both discovered building it:

- **Every run is addressed by an explicit run ID, never by `""`.** Step 2 is the only place the
  empty run ID would be convenient, and using it there breaks truncation detection: the server
  silently resolves `""` to the *latest* run, so a request for a reaped predecessor would
  cheerfully return the newest run's events instead of reporting the predecessor as gone — and
  the walk would loop on the same run forever. Step 1 exists purely to bootstrap a real run ID
  for step 2.
- **`isLongPoll` must be false.** With it true the iterator on a running workflow blocks waiting
  for events that have not happened yet, so the audit page for an active customer hangs rather
  than returning what exists now.

### The crawl needs no worker, and the detail page does

**The audit crawl is the most available read in the system, and the detail page the least.**
Counterintuitive given the crawl is O(runs) and the detail page is one Query — but the crawl
only reads persistence, while a Query needs a worker to replay history. Measured with
`make worker-stop`: `/audit` answers in 10 ms while `/api/customers/{id}` 503s.

Taking `LifetimeEarnEvents` from the newest run's start payload rather than from a Query is what
buys this, and it costs nothing.

**Cost:** the crawl is O(runs × events) per page view, uncached, and serial — each run only
learns its predecessor from the run just read, so the round trips cannot be issued in parallel.
Measured, and cheaper than expected: a customer with 34 runs and 100 point-adds crawls end to end
in **~125 ms**, warm or cold.

There is deliberately **no cap on runs walked**, only a 30 s deadline on the whole request. A cap
would have to report its partial result as `truncated`, which in this contract means "history was
deleted" — and quietly widening that to "or we gave up" would make the one honest signal in the
response dishonest.

### Events the crawl reads

| Event | Audit entry |
|---|---|
| `WorkflowExecutionStarted` (with empty `ContinuedExecutionRunId`) | Enrolled |
| `WorkflowExecutionStarted` (with a non-empty one) | generation boundary (rendered as a subtle divider) |
| `WorkflowExecutionUpdateAccepted` | the request: update name, `Amount`, `Reason`, update ID |
| `WorkflowExecutionUpdateCompleted` | the outcome: new balance and level, or failure message |
| `ActivityTaskScheduled` + `ActivityTaskCompleted` | Promotion notification sent |
| `WorkflowExecutionUpdateCompleted` (`deactivate`, `changed: true`) | Deactivated |
| `WorkflowExecutionUpdateCompleted` (`reactivate`, `changed: true`) | Reactivated |

Accepted and Completed are separate events; pair them via `AcceptedEventId` on the completed
event to render one row with both the request and its result. Payloads are `Payloads` protos —
decode with the same `DataConverter` the client is configured with, which is why API and worker
share a Go module.

Three corrections to the table as originally predicted:

- **The generation boundary is read from the successor, not the predecessor.** The obvious
  choice is `WorkflowExecutionContinuedAsNew`, which is the last event of the run being *left*.
  Both events mark the same instant, but only the successor's `WorkflowExecutionStarted` knows
  which generation is being entered — and, more to the point, the predecessor is the half that
  gets reaped. Reading the boundary from the run that still exists is what lets a truncated log
  still show "generation 33" at the top instead of starting mid-air.
- **`WorkflowExecutionStarted` carries the whole carried `CustomerState` as its input**, which
  turns out to be the most useful event in the crawl. It gives the generation and
  `LifetimeEarnEvents` for free — no Query needed, and therefore no worker.

  **The search attributes on that same event are the *predecessor's*.** A run upserts its own on
  its first workflow task, so the started event of generation 2 carries `RewardsGeneration: 1`.
  The event's input payload is the carried state and is correct; the attributes on it are a
  snapshot of the run being left. Read the input, not the attributes.
- **A notification row needs `ActivityTaskCompleted`, not `ActivityTaskScheduled`.** "Sent" has
  to mean sent: an Activity that exhausted its retries leaves a Scheduled event and a Failed one,
  and rendering the first as a delivery would make the audit log lie about the one thing it
  exists to be believed about. The Scheduled event carries the input, the Completed one carries
  `ScheduledEventId` — so pair them exactly like the Update halves.

Notification rows land in the audit log almost for free — but not *quite*: the crawl has to know
the Activity's name to tell a notification from any other Activity, and has to decode its
argument to get the level. `ActivityNotifyCustomer` and `NotifyRequest` therefore live in
`internal/rewards/notify.go` rather than beside the Activity, so the shape cannot change without
the crawler failing to compile.

### Truncation detection

Closed runs get reaped, and the audit log is designed around that rather than pretending it
won't happen. Walking back, a non-empty `ContinuedExecutionRunId` whose `GetWorkflowHistory`
fails means history was reaped: stop and mark the result truncated.

**It is not `NotFound`, which is what was predicted**, and getting that wrong is not cosmetic —
truncation is detected *by this error*, so with the predicted classification the one case this
exists to handle came back as an unmapped 500. Measured:

| condition | Go type | message |
|---|---|---|
| run reaped | `*serviceerror.InvalidArgument` | *Requested workflow history not found, may have passed retention period.* |
| run ID well-formed, never used | `*serviceerror.InvalidArgument` | identical to the above |
| run ID malformed | `*serviceerror.InvalidArgument` | *Invalid RunId.* |
| workflow ID never existed | `*serviceerror.NotFound` | *workflow not found for ID: …* |

So the type alone cannot decide it, and `isHistoryGone` is **the one place in the codebase where
message text decides an outcome** — the only other text match, `mentionsNoPoller`, merely picks
the wording of a 503 whose status is already decided. Anywhere else that would be a bug. There is
no other signal: from the server's side, "history deleted" and "run ID you made up" are the same
situation. That is only tolerable because the crawl exclusively passes run IDs the server itself
produced in a `ContinuedExecutionRunId`, which makes the second row unreachable.

Note the server says *may have passed retention period* even for a run deleted explicitly by
`make reap`, where that guess is simply wrong. If a server upgrade changes the wording,
truncation stops being recognised and surfaces as a 500 — the right direction to fail in, since a
loud error beats a timeline quietly showing fewer rows than the customer has.

The carried `CustomerState` gives ground truth to *quantify* the gap. If `LifetimeEarnEvents` is
23 and only 7 rows could be reconstructed, the UI says:

> Showing 7 of 23 point events. Earlier history has been deleted.

`EnrolledAt` and the running totals survive in the continue-as-new payload, so the header of the
detail page stays fully correct even when the log beneath it is partial.

This is the honest version of "Temporal as a data store," and demonstrating the limitation is
more valuable than hiding it.

#### What a reaped customer looks like

Measured end to end:

| state | `GET /{id}` | `GET /{id}/audit` |
|---|---|---|
| active, intact history | 200 | 200, `truncated: false` |
| active, old generations reaped | 200 | 200, `truncated: true`, e.g. shown 1 of 100 |
| soft-deactivated, not reaped | 200 | 200, ending in a `deactivated` row |
| re-enrolled after leaving | 200 | 200, `deactivated` then `reactivated`, balance carried through |
| ops-closed then fully reaped | 404 | 404 |

The middle active row is the demo: `make reap WF=customer-x` on an *active* customer leaves the
running generation and deletes every closed one, so the header still reads 100 lifetime
point-adds while the timeline beneath it can only show the one that survives.

**Soft-deactivated customers do not vanish on retention.** They stay `Running`, so neither the
1 h reaper nor `make reap` (which filters `ExecutionStatus != "Running"`) touches them. What
*does* get reaped are rolled-over generations and any ops-closed (`Canceled` / `Terminated`)
executions.

---

## Versioning and replay

### Versioning is the real risk

Entity workflows run forever and will outlive deploys; changing workflow code under a running
execution causes non-determinism errors.

**Confirmed, and it had already happened.** Adding the notification Activity shipped a change
that would have wedged every customer with an open run. The new `ScheduleActivityTask` command
has no corresponding event in histories recorded by the previous build, so replay fails, the
workflow task retries forever, and the customer is stuck — with no error surface anywhere a user
or an operator would look:

```
nondeterministic workflow: extra replay command for ScheduleActivityTask:
  (ActivityType:(Name:NotifyCustomer) ...)
```

**Nothing else caught it.** Unit tests passed, the API worked, the UI rendered; the damage only
exists for executions that started *before* the deploy, and the only thing that looks at those is
a replay test. That is the whole argument for having one.
`internal/rewards/workflows/testdata/pre-notification-*.json` are real histories recorded by the
earlier
worker, so replaying them is a rehearsal of that deploy.

Fixed with `workflow.GetVersion` gating the Activity, so runs recorded before the marker keep the
old behaviour for the rest of their lives and pick notifications up at their next
continue-as-new — at most `EarnsPerRun` adds away.

**And the fix has a population it cannot save, which is the sharper half of the lesson.**
Executions created by the *ungated* build — the code as it actually merged — have the Activity in
their history and no marker. They resolve to `DefaultVersion` exactly like a pre-change run, so
replay omits an Activity the history demands:

```
lookup failed for scheduledEventID to activityID: scheduleEventID: 24
```

`GetVersion` cannot distinguish "predates the change" from "ran the change before it was gated":
the marker is the only signal and neither has one. Whichever way `DefaultVersion` is interpreted,
one population breaks. Gating is still right — it protects everyone from before the deploy, at the
cost of those started between two commits — but no later commit can reach back and repair the
histories the ungated build wrote. Pinned by
`TestReplay_UngatedPhase6HistoriesCannotBeRescued`.

The affected executions are at least findable, because `GetVersion` upserts
`TemporalChangeVersion`:

```
WorkflowType = 'CustomerRewardsWorkflow'
  AND ExecutionStatus = 'Running'
  AND TemporalChangeVersion IS NULL
  AND StartTime > '<when the ungated build went out>'
```

The `StartTime` clause is what separates them from earlier runs, which also lack the marker and
replay perfectly well. Then reset them — `make reset` in dev, a targeted terminate in anything
real.

**The lesson is upstream of all of it: gate a command-changing edit in the same commit that
introduces it.** There is no later commit that can fix having not done so.

### GetVersion writes two events

Not one: the `Version` marker *and* an automatic upsert of the built-in `TemporalChangeVersion`
search attribute. The second is a gift — executions become queryable by which version of the code
they are running, which is exactly what you want during a migration:

```
WorkflowType = 'CustomerRewardsWorkflow' AND TemporalChangeVersion = 'tier-notifications-1'
```

That answers "how many customers have picked up the change yet?" from the Temporal UI, with no
instrumentation of our own.

### Replay substitutes a fake workflow ID

`ReplayWorkflowHistory` runs the workflow as `"ReplayId"` unless `ReplayWorkflowHistoryWithOptions`
is given an `OriginalExecution`. Any workflow that reads its own ID therefore behaves differently
under replay — ours rejects the enrollment payload
([the integrity boundary](#the-workflow-is-the-integrity-boundary)), returns before emitting a
single command, and reports:

```
nondeterministic workflow: missing replay command for UpsertWorkflowSearchAttributes
```

Which names a line that is entirely innocent, and says "nondeterministic", so it reads as a
versioning problem rather than a harness one. The option's doc comment calls it "Optional". It is
optional only for workflows that never look at their own identity.

### Stale workers

**Stale processes are invisible and look exactly like a code bug.** This cost real debugging
time: continue-as-new was correct and unit-tested, but an old worker kept winning the tasks, so
`generation` stayed 0 and the feature looked broken.

The worker used to run on the host under `go run`, which execs its binary out of
`/root/.cache/go-build/<hash>/worker` — a path containing neither `cmd/worker` nor `exe/worker`
— so the obvious `pkill -f cmd/worker` left it alive and happily polling the same task queue with
the *old* code. That is why the worker and the API are now Compose services: `make worker` and
`make api` rebuild the image and recreate the container in one step, and `make ps` shows the one
of each that exists. There is no build cache path to miss and no way to end up with two.

The shape of the trap survives the move, in a milder form: an image is a snapshot, so a container
started before an edit still serves the old code until it is rebuilt. It is now visible
(`make ps` gives the container's age, `make worker-logs` its startup line) rather than silent.

Worth pairing with the versioning discipline above: stale *workflows* and stale *workers* fail in
opposite directions — one errors loudly on replay, the other succeeds quietly with the wrong
logic.

---

## The local stack

`postgres` (persistence), `elasticsearch` (visibility), `temporal` (auto-setup image),
`temporal-ui`, `worker`, `api` and `web` in Compose, plus `seed` as a one-shot behind a profile.
Nothing runs on the host. The three Go services share one `deploy/Dockerfile`, selected by a
`CMD` build arg, and the UI is the stock `node` image with `web/` bind-mounted, so `make up`
needs Docker and no toolchain at all.
`temporalio/auto-setup` with `DB=postgres12`, `ENABLE_ES=true`, `ES_SEEDS=elasticsearch` creates
schemas and installs the ES index template for us.

Operational scripts run via `exec` into the **server** container rather than a separate
`admin-tools` one: the `auto-setup` image already ships the `temporal` CLI, which saves several
seconds per invocation and one more image to pin. It has `bash`, `curl` and `grep` but **no
`python3` or `jq`**, which constrains how those scripts are written.

Compose can't do arithmetic, so rather than a port-offset scheme, `.env` names every port
explicitly and `COMPOSE_PROJECT_NAME` isolates containers, networks, and named volumes. **A
cheaper alternative to a whole second stack is a second namespace** on one stack — isolated
workflows and search attributes for a fraction of the RAM. Full stack duplication is worth it
only for testing server config changes.

### Retention has a one-hour floor

The original design set namespace retention to 20 minutes so reaping happened while you watched.
**That is not achievable**, and the finding is worth recording because it is not what the
documentation implies:

- Temporal enforces a **1 hour minimum** namespace retention. Probing server 1.29.7 directly,
  `59m` is rejected with *"A valid retention period is not set on request"* and `1h` is accepted.
- The dynamic config key that would lower the floor, `system.namespaceMinRetentionLocal`, **exists
  only on unreleased `main`**. Setting it on 1.29.7 produces
  `dynamic config warning ... unregistered key "system.namespaceMinRetentionLocal"` at startup,
  after which it is loaded and then ignored — so it looks configured but does nothing. 1.29.7 is
  the newest published `auto-setup` image, so no released server has it.

So retention sits at the 1 h floor, and truncation is forced on demand instead:

```sh
make reap                      # every closed execution in the namespace
make reap WF=customer-abc123   # just that customer's closed runs
```

Implemented with `temporal workflow delete --query`, a server-side batch operation. Verified end
to end: a closed execution stops resolving roughly **25–40 s** after the request, so deletion is
asynchronous and a demo should expect a short pause rather than an instant change.

**The `ExecutionStatus != "Running"` filter in that query is load-bearing, not a nicety.**
`workflow delete` *terminates* a running execution before deleting it, so an unfiltered reap
would destroy every active customer rather than just their old generations.

Arguably this is the better demo anyway: it makes truncation a thing you *do* on cue rather than
a thing you wait out, and it targets one customer so the contrast between a truncated and an
intact audit log is visible side by side.

The reaping timer is jittered, so natural deletion of closed runs lands somewhere in
`[retention, retention + jitter]` rather than exactly on the hour.
`history.retentionTimerJitterDuration` **is** a registered key (verified against the 1.29.7 binary
and by the server logging it applied), and dev config pins it to 1m.

`make verify-config` asserts all of the above — the 1 h floor, that every key in
`dynamicconfig/dev.yaml` is genuinely registered, and that on-demand deletion works. **Re-run it
after a server upgrade**; if a future version relaxes the floor, check 1 fails loudly and the
workaround can be dropped.

### Making Elasticsearch cheap

This stack holds tens of workflows, not millions, so ES can be tuned well below its defaults.
Starting point is Temporal's own reference Compose, which already sets a 256 MB heap and —
importantly — **absolute-byte disk watermarks** rather than percentages.

| Lever | Effect |
|---|---|
| `ES_JAVA_OPTS=-Xms256m -Xmx256m` | The dominant term. 256 MB is the practical floor; 128 MB sometimes boots but GC-thrashes, so treat it as an experiment, not a default. |
| `mem_limit: 768m` | Heap is only part of RSS — JVM overhead plus Lucene's off-heap structures add roughly 2×. Caps the blast radius so a misbehaving ES can't take the laptop with it. |
| `xpack.ml.enabled=false` | ML spawns native processes that allocate *outside* the heap. Pure waste here. Not in Temporal's default compose; worth adding. |
| `xpack.monitoring.collection.enabled=false`, `xpack.watcher.enabled=false` | Background indexing into system indices we will never read. |
| `indices.memory.index_buffer_size=5%` | Default is 10% of heap held for indexing buffers. Our write volume is a handful of docs per minute. |
| `discovery.type=single-node` | Skips cluster bootstrap and quorum machinery. |

**Shards and replicas need no attention.** Temporal's own visibility index template already sets
`number_of_shards: 1` and `number_of_replicas: 0` (plus `auto_expand_replicas: "0-2"`, which stays
at 0 on a single node). The usual single-node "cluster stuck at yellow" trap doesn't apply — it is
already handled upstream.

**Keep the absolute-byte watermarks.** The default *percentage* watermarks (85/90/95% of disk) are
a nasty failure mode on a developer laptop: a nearly-full disk flips ES indices to read-only, and
then **visibility silently stops updating while workflows keep running perfectly**. The customer
list freezes, the detail pages stay correct, and nothing logs an obvious error. Temporal's
byte-based defaults avoid it:

```yaml
- cluster.routing.allocation.disk.threshold_enabled=true
- cluster.routing.allocation.disk.watermark.low=512mb
- cluster.routing.allocation.disk.watermark.high=256mb
- cluster.routing.allocation.disk.watermark.flood_stage=128mb
```

Realistically this lands ES around **500–700 MB RSS**. Since ES is the only visibility store, that
is the floor per stack — if several stacks at once get tight, the lever is running fewer stacks or
using a second namespace on one stack, not further ES tuning.

(OpenSearch is a supported drop-in alternative, but it is not meaningfully lighter, so it isn't
worth the substitution here.)

**Elasticsearch and Temporal server versions must be compatible** (ES 7 needs Temporal 1.7+, ES 8
needs 1.18+). Both images are pinned.

---

## Smaller edges

### workflow.Now, never time.Now

Same for randomness and UUIDs.

### AllHandlersFinished covers handlers, not goroutines

It tracks Update and Signal handlers
only, *not* `workflow.Go` goroutines. Any background work spawned that way needs its own drain
condition before continue-as-new or workflow completion, or it is silently dropped.

Confirmed, and not a corner case. The first version of notification delivery used a `workflow.Go`
goroutine draining a queue, and the pre-continue-as-new await would happily roll the run while a
notification was still in flight. Written as a test before the guard existed — three adds where
the third crosses into gold, the ordinary shape at `EarnsPerRun = 3` — and the promotion vanished:

```
--- FAIL: Test_Notify_PromotionOnTheRollingAddIsNotDropped
    expected the promotion to survive the roll, got []
```

No error, no retry, no trace in history: the run rolled while the notification sat in a queue that
`AllHandlersFinished` knows nothing about. The fix at the time was an extra `notifier.Idle()`
clause on the roll condition.

**The code no longer relies on any of that, and the reason is the real lesson.** Delivery moved
into the workflow's main loop, so there is no side goroutine for `AllHandlersFinished` to miss and
no guard to forget — the trap was removed rather than defended against. A `workflow.Go` goroutine
buys concurrency the workflow did not need, and charges for it in an invariant nothing enforces.
Reach for the loop first; this still stands for the cases where you genuinely cannot. The test
remains the thing that catches a regression.

### Await returning nil does not mean not cancelled

`workflow.Await` evaluates its condition *before* it checks the context:

```go
for !condition() {
    ... return NewCanceledError(...) if ctx is done ...
    state.yield("Await")
}
return nil
```

so a condition that already holds short-circuits the cancellation check entirely. Any `Await` whose
nil return is treated as "the condition fired, and only the condition" is wrong whenever
cancellation can race it. Here that meant a cancel arriving in the same workflow task as the Nth
point-add would roll the run instead of deactivating — and strand the departure permanently,
because continue-as-new starts a fresh run while the cancellation targeted the run that just
ended. The customer clicks deactivate and stays active.

**The platform fact is permanent; the prescription is not.** Soft deactivation removed the race by
removing the racer — leaving is an Update, nothing cancels the workflow, and the `ctx.Err()`
re-checks came out. The mechanic still bites any `Await` in a workflow that *does* take
cancellation seriously.

### Background work that must outlive cancellation needs a disconnected context

While
leaving was a cancellation, the notifier ran on one, so that deactivating a customer mid-delivery
did not cancel a promotion they had already earned. Running it on the workflow's own context looks
natural and silently loses that notification. No longer load-bearing here — nothing cancels the
workflow now — but recorded because the failure is silent, which is what makes it worth knowing
before you need it.

### Status means two different things, in two different vocabularies

The visibility query
language says `ExecutionStatus = 'Running' | 'Canceled'`; the API's DTOs say
`status: "active" | "deactivated"`. Same English word, no overlap in spelling — and they are not
interchangeable. Under soft deactivation membership is `RewardsActive`, so a status filter in the
UI has to translate `active` ↔ `RewardsActive = true` (and still exclude `ContinuedAsNew`).
`ExecutionStatus` alone cannot answer "is this customer still in the program?"

### The Go API sends no CORS headers

Pointing a browser at it with a cross-origin base URL fails; the `web` service proxies `/api`
through Vite instead (`VITE_API_PROXY_TARGET=http://api:8081`, a hop that happens inside the
compose network rather than in the browser).
Left as-is rather than "fixed" by adding permissive CORS: same-origin proxying is the normal Vite
setup and the one that survives into production, whereas an unauthenticated API advertising
`Access-Control-Allow-Origin: *` is a shape worth not copying out of a POC.

### Concurrent point-adds cannot lose an update

Updates are serialized by the workflow — no
optimistic locking, no transactions, no retry loop. This is a genuine advantage over the obvious
Postgres implementation.

### No authentication, and no payload encryption

Customer names and emails land in Event History and are readable in plaintext in the Temporal
UI. Fine for a POC; the production answer is a Codec Server. Use obviously fake seed data.

There is no authentication anywhere either. Nothing here is a starting point for something
exposed.

---

## Out of scope

Auth, multi-tenancy, points spending, tier expiry, archival, Codec Server, Worker Versioning,
production deployment, pagination and caching of the audit crawl, and horizontal scaling of
workers.

Points spending is now explicitly *decided against* rather than merely deferred — see
[points only go up](#points-only-go-up). Tier downgrade over time and tier-anniversary review
would go in the same place: the entity workflow with a durable timer is exactly the shape that
buys them cheaply.

An **integration test** was planned and never built, because there is no CI in this repo:
`temporal server start-dev` with `--search-attribute` flags, exercising the API end to end. It
would run without Docker or Elasticsearch — but note `start-dev` uses SQLite visibility, so it
would not reproduce the [visibility lag](#visibility-lag). That behaviour needs the real stack.
