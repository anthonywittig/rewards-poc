# rewards-poc

Demonstrating **Temporal as the system of record** for a customer rewards program.

There is no application database for rewards state. A customer's points, tier, enrollment
date, and history of point-earning events live entirely in a Temporal Workflow Execution and
its Event History. See [docs/PLAN.md](docs/PLAN.md) for the full design.

**Complete.** The workflow runs, continues-as-new every 3 point-adds, notifies customers when
they reach a tier, and is drivable from the `temporal` CLI, over HTTP, or through the React UI.
The customer list comes straight out of the visibility store; the audit timeline is
reconstructed by crawling Event History.

Every phase of this project found something the plan got wrong, and those corrections are the
most useful thing in it — they're collected in [§12 of the plan](docs/PLAN.md#12-sharp-edges).
The one worth reading first is §12.11.

## Quick start

Requires Docker with Compose v2. Roughly 1.5 GB of images and ~1 GB of RAM. Running the worker
also needs Go — 1.25.4 or newer, which is the Temporal Go SDK's own floor, not ours (on an
older Go the default `GOTOOLCHAIN=auto` will fetch it for you).

```sh
make up
```

Defaults come from `.env.example`. Copy to `.env` only when you want local overrides.

That starts Postgres, Elasticsearch, Temporal, and the Temporal Web UI, then bootstraps the
namespace and search attributes. When it finishes:

- Temporal UI: <http://localhost:8080>
- Temporal gRPC: `localhost:7233`, namespace `rewards`

```sh
make help      # all targets
make ps        # stack status
make down      # stop, keep data
make destroy   # stop and delete volumes
```

## Driving the workflow

Start the worker in one terminal and leave it running:

```sh
make worker
```

Then, from another:

```sh
make enroll ID=c-001 NAME="Ada Lovelace" EMAIL=ada@example.com
make status ID=c-001
make add    ID=c-001 AMOUNT=499 REASON=purchase   # basic
make add    ID=c-001 AMOUNT=1   REASON=purchase   # -> 500, promoted to gold
make deactivate ID=c-001                          # soft leave; the workflow keeps running
make reactivate ID=c-001 NAME="Ada Lovelace" EMAIL=ada@example.com   # rejoin, balance intact
make status ID=c-001                              # still 500 points, gold
```

Re-enrollment takes the name and email it is given, so pass them unless you mean to change
them — the target's defaults are a convenience for throwaway IDs, not a no-op.

`make test` runs the unit tests; they need neither Docker nor a running server.

**If the workflow seems to ignore a code change, check for a stale worker first.** `go run`
execs its binary out of the Go build cache, at a path containing neither `cmd/worker` nor
`exe/worker`, so the obvious `pkill` misses it and it keeps serving *old* code against the same
task queue — silently, and looking exactly like a bug in the workflow:

```sh
make workers       # should list exactly one
make worker-stop   # kills them all, including orphans
```

This cost real debugging time while building Phase 2. Stale *workflows* fail loudly on replay;
stale *workers* succeed quietly with the wrong logic, which is much worse.

## Continue-as-new

Every 3 successful point-adds, the workflow ends its Run and immediately starts a fresh one
carrying its state forward. Watch it happen:

```sh
make enroll ID=roll NAME="Rolly Poly" EMAIL=r@example.com
for i in 1 2 3 4 5 6 7; do make add ID=roll AMOUNT=100 REASON="add $i"; done
make status ID=roll     # generation 2, points 700
```

The balance accumulates across the boundary while `generation` ticks up. `temporal workflow
list --query "WorkflowId = 'customer-roll'"` shows the chain: one `Running` run and two
`ContinuedAsNew` ones. The current run's history is under a dozen events, against the ~47 the
same seven adds accumulate without rolling — which is the entire point, since history has hard
limits (50k events / 50 MB) and an entity workflow is meant to live for years.

**Three is a demo number, not a defensible rule.** It's hardcoded because it makes the rollover
easy to watch in a terminal — but what actually matters is history *size*, and a count of adds
is only a proxy for it. Three is wastefully early for small updates and would be far too late
if each add carried a large payload; the real limits are 50k events and 50 MB per run, neither
of which is a number of adds.

Production should ask the server, which already tracks the real thing:

```go
workflow.GetInfo(ctx).GetContinueAsNewSuggested()
```

It flips to true as a run approaches those limits, and `GetContinueAsNewSuggestedReasons()`
says which one. That also removes the hazard below entirely — there's no constant left to
change.

**Changing `EarnsPerRun` breaks running workflows.** A run whose history records a roll after 3
adds will not produce that command at that point when replayed under a different value, and a
command that doesn't match the recorded event is what the replayer refuses. Entity workflows
outlive deploys, so this isn't theoretical. In dev, terminate existing workflows after changing
it.

One customer is one long-lived Workflow Execution with the ID `customer-<id>`, which is why
none of these commands need a lookup table. Points arrive as Updates, status is a Query, and
so is leaving: deactivation is an Update that sets a flag, not a cancellation.

**Re-enrolling a deactivated customer restores their balance.** Leaving is soft — the execution
stays Running with `deactivated` set — so the workflow ID is still occupied and re-enrollment is
an Update against the same execution rather than a fresh Start. Points, tier and
`lifetimeEarnEvents` are all still there, and the timeline shows a `deactivated` row followed by
a `reactivated` one. See §3.6 of the plan.

Two consequences worth knowing. A customer who was *cancelled* out of band (`temporal workflow
cancel`) is genuinely closed, and re-enrolling them does start over at zero — that is the ops
path, not the product one. And membership no longer lives in `ExecutionStatus`: it is the
`RewardsActive` search attribute, which is what the list and its filter chips read.

## The HTTP API

Run the worker and the API in two terminals:

```sh
make worker
make api        # :8081
```

Building the UI? With the stack up, run `make api` and `make web` — Vite proxies
`/api` to `:8081` by default.

**The customer list is capped at five rows and has no pagination.** That's a consequence of
`ORDER BY` not working: with no stable ordering, "page 2" doesn't mean anything in particular, so
the list returns a small slice, tells you how many matched, and pushes you to filter — which is
what the visibility store is actually good at.

## The list is the visibility store, directly

`GET /api/customers` is a `ListWorkflow` plus a `CountWorkflow` and nothing else — no lookup
table, no local index. The `?q=` parameter is passed to Temporal essentially as typed:

```sh
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsLevel = 'gold'"
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsPoints >= 500"
curl -sG localhost:8081/api/customers --data-urlencode "q=CustomerName = 'Ada'"        # Text, partial
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsActive = false"       # deactivated
```

**The same query works unchanged in the Temporal UI**, which is the point of registering search
attributes at all. Verified both ways:

```
our API                     "total":12
temporal workflow count     Total: 12
```

Responses carry `total` and `complete`, so the UI renders *"Showing 5 of 23 — filter to find
additional results"* and only offers sorting when it has the whole set.

A rejected query comes back as a 400 carrying Temporal's own diagnostics, which are better than
anything worth paraphrasing:

```
invalid search attribute: NoSuchAttribute
invalid value for search attribute RewardsPoints of type Int: "not-an-int"
```

`ORDER BY` is the one exception — caught before the query is sent, because scoping the caller's
filter turns Temporal's clear "not supported" into a bare syntax error. You get an explanation
and what to do instead.

```sh
curl -XPOST localhost:8081/api/customers \
  -d '{"customerId":"c-001","name":"Ada Lovelace","email":"ada@example.com"}'

curl localhost:8081/api/customers/c-001
curl -XPOST localhost:8081/api/customers/c-001/points -d '{"amount":500,"reason":"purchase"}'
curl -XDELETE localhost:8081/api/customers/c-001
```

The API holds a Temporal client and **nothing else** — no database, no cache, no ORM. That
absence is the whole argument of the POC, and `internal/httpapi` is short enough to confirm it
at a glance.

Every failure comes back as `{"error":{"code":"...","message":"..."}}` with a stable code:

| | Code | When |
|---|---|---|
| 400 | `invalid_request` | malformed body, missing `customerId`, unknown JSON field |
| 404 | `not_found` | no such customer, or their history was reaped |
| 409 | `already_exists` | enrolling a customer who is already active (a deactivated one is reactivated instead, 200) |
| 409 | `deactivated` | adding points to a customer who has left |
| 409 | `rollover_race` | the workflow rolled over twice while applying one request |
| 422 | `rejected` | the workflow refused it — see the split below |
| 503 | `worker_unavailable` | nothing is polling the task queue |

The 503 is the one you'll meet most, because the worker is down more often than anything else
in development. It says so in as many words rather than surfacing a gRPC error.

Two things behind that table were only learnable by measurement, and are worth knowing before
you build anything else on Temporal:

- **A Query with no worker fails three different ways** depending on how long the worker has
  been gone — a bare gRPC `RST_STREAM` in the first seconds, then `FailedPrecondition` or
  `DeadlineExceeded`, all taking ~9–10s. The API bounds queries at 2s so its own deadline wins
  and all three become one predictable 503.
- **An Update with no worker doesn't fail at all — it blocks.** Still waiting after two
  minutes. Without a bound, `POST /points` hangs for as long as the client holds the
  connection.

Same underlying condition; fails fast on one API, never on the other.

## The audit log is the Event History

`GET /api/customers/{id}/audit` is the endpoint the whole POC is arranged around. Every other
read is served by something that behaves like a database — a Query against live state, or the
visibility index. This one is served by *the log itself*: nothing stores a customer's history
of point-adds, it is reconstructed by walking back through the run chain and reading the events
Temporal recorded because it had to in order to run the workflow at all.

```sh
make audit ID=c-001
```

```
enrolled           gen=0 ev=1
points_added       gen=0 ev=6   +1000 -> 1000 (platinum) 'purchase 1'
points_added       gen=0 ev=12  +1000 -> 2000 (platinum) 'purchase 2'
points_added       gen=0 ev=18  +1000 -> 3000 (platinum) 'purchase 3'
generation_rolled  gen=1 ev=1
...
deactivated        gen=2 ev=9
```

### Truncation is a feature, not an error

Closed runs get reaped, so the log is not durable the way a table would be — and the response
says so rather than quietly showing less:

```sh
make reap WF=customer-c-002    # deletes the closed generations, keeps the running one
make audit ID=c-002
```

```
truncated=True shown=1 lifetime=100 runsWalked=1
```

*"Showing 1 of 100 point events. Earlier history has been deleted."* The header of the detail
page stays completely correct while the timeline beneath it cannot: the totals ride in the
continue-as-new payload, which is exactly what §6.3 of the plan is about. Demonstrating the
limitation is worth more than hiding it.

Two things measured while building this that were not obvious:

- **The crawl needs no worker.** It is the most expensive read here — one round trip per
  generation, serially — and also the most available, because it only talks to the server.
  With `make worker-stop`, `/audit` answers in 10 ms while the detail page 503s.
- **It is cheap.** A customer with 34 runs and 100 point-adds crawls end to end in ~125 ms.

## The validator/handler split, live

This is the one behaviour worth seeing rather than reading about. Both of these fail, and the
caller cannot tell them apart — but only one leaves a trace.

```sh
make add ID=c-001 AMOUNT=-50 REASON=oops     # rejected by the validator
make add ID=c-001 AMOUNT=1001 REASON=toobig  # rejected by the validator
```

Now count the history events before and after in the Temporal UI, or with
`temporal workflow show --workflow-id customer-c-001`. **The count does not change.** A
validator rejection writes nothing at all — no events, no audit row. A client stuck in a retry
loop sending `amount: -1` cannot grow history by a single event.

Compare a rejection that comes from the *handler* instead — one that depends on the customer's
accumulated state rather than the shape of the request:

```sh
# a customer near the 100,000 points cap
make add ID=c-002 AMOUNT=11 REASON="over cap"
```

That one appends `WorkflowExecutionUpdateAccepted` and `WorkflowExecutionUpdateCompleted`, so
the denial is permanently recorded and shows up in the audit log as a `points_rejected` row.
Which is the point: *"why didn't this customer reach platinum?"* has an answer, while
*"someone's integration sent a negative number 4,000 times"* does not clutter the record.

Measured on a live customer: two validator rejections left the audit log at 135 rows; one
handler rejection took it to 136.

The rule of thumb, and where each rejection lives in the code:

| | Where | Writes history? | Example |
|---|---|---|---|
| Facts about the **request** | Update validator | No | negative amount, over per-txn max, missing reason |
| Facts about the **customer** | Update handler | Yes | would exceed the points cap |

## The UI

```sh
make up && make worker && make api
make web         # :5173 — /api proxied to :8081
```

Vite proxies `/api` to whatever `VITE_API_PROXY_TARGET` points at, defaulting to the real API.

It has to be a proxy rather than a cross-origin base URL: the Go API deliberately sends no
CORS headers, and same-origin proxying is both the normal Vite setup and the one that survives
into production. Only the mock sets CORS, because it exists to be hit directly with no stack.

See `web/NOTES.md` for the UI's own notes.

## The one Activity

`NotifyCustomer` is the only thing in this codebase that would touch the outside world.
Everything else — points, tiers, enrollment, the audit log — is workflow state and needs no
side effects at all, which is rather the argument. Having exactly one Activity makes the
boundary visible.

It fires when a point-add leaves the customer at a tier they have not been told about, and
again when a customer leaves. The handler does **not** await it: it applies the points, arms a
flag and returns, so a notification provider being down can neither fail a point-add that is
already recorded nor put a network call on the UI's critical path.

That condition is deliberately about the customer's *state* rather than about the add that
just happened. "Did this add cross a boundary" is an event — it occurs once and is then gone,
so a delivery that exhausted its retries could never be attempted again. "Is this customer at
an unannounced tier" is a property, so the next add picks it up.

Delivery happens in the workflow's main loop, which runs **cancel → notify → continue-as-new**
in that order and re-enters after each step. Departure always wins over a roll; a pending
promotion is always sent before one.

It did not start that way, and the detour is the most instructive thing in the design. The first
version drained a queue from a `workflow.Go` goroutine, which walks straight into:

> **`workflow.AllHandlersFinished` does not cover `workflow.Go` goroutines.** It tracks Update
> and Signal handlers only.

So the pre-continue-as-new drain that Phase 2 added was not enough. A promotion landing on the
third add — the ordinary case at three adds per run — was queued, the handler returned,
`AllHandlersFinished` went true, and the run rolled away from a notification nobody ever sent.
No error, no retry, no trace in history.

The test for it was written before the fix, and failed:

```
--- FAIL: Test_Notify_PromotionOnTheRollingAddIsNotDropped
    expected the promotion to survive the roll, got []
```

That was first patched with an extra `&& n.idle()` clause on the roll condition. Moving delivery
into the main loop removed the need for it entirely: with no side goroutine, there is nothing
for `AllHandlersFinished` to miss and no invariant to remember. The platform fact is still worth
knowing; it is just no longer load-bearing here.

```sh
make enroll ID=c-003
make add ID=c-003 AMOUNT=200 REASON=purchase   # x3, crossing gold on the third
make audit ID=c-003
```

```
points_added       gen=0 +200 -> 600 (gold)
notification_sent  gen=0 level=gold
generation_rolled  gen=1
```

The notification row lands *before* the generation divider, which is the guard doing its job.
And it appears in the audit log for free, because Activities are history events — the crawl
needed no changes at all to render it.

## Points only go up

There is no spending, redemption, expiry, or manual adjustment, and none is planned. `addPoints`
is the only thing that writes a balance and it only ever adds, so points are monotonic for the
life of a customer.

Two consequences worth knowing before you go looking for them:

- **Tiers never demote.** They're derived from a monotonic balance, so they're monotonic too.
  The only way down is to raise a threshold, which demotes everyone at once.
- **Deactivating and re-enrolling is the one thing that resets a balance**, and it does so by
  starting a new execution rather than mutating one. The old balance leaves with the old run.

This is also why there's a single `Points` field and no separate lifetime total — with a
monotonic balance those are the same number. See §3.1 of the plan for why carrying both was
worse than carrying one.

## The replay test is the one that matters

```sh
go test ./internal/rewards/ -run TestReplay
```

A customer's workflow runs forever, so it **will** outlive a deploy. When a worker picks up a
task for a run that started weeks ago, it replays that run's recorded history through today's
code and requires the commands to match event for event. If they don't, the workflow task fails
and retries forever: the customer is wedged, silently, and restarting nothing helps.

`internal/rewards/testdata/pre-notification-*.json` are real histories recorded by the *Phase 5*
worker, before the notification Activity existed. Replaying them is a rehearsal of the deploy
that added it — and the first time it ran, it failed:

```
nondeterministic workflow: extra replay command for ScheduleActivityTask:
  (ActivityType:(Name:NotifyCustomer) ...)
```

Adding the Activity would have wedged **every customer with an open run**. Nothing else caught
it: the unit tests passed, the API worked, the UI rendered. The damage only existed for
executions that started before the deploy, and the only thing that looks at those is a replay
test.

The fix is `workflow.GetVersion`. Runs whose history predates the marker keep the old behaviour
for the rest of their lives and pick notifications up at their next continue-as-new — at most
three adds away.

**It arrives one commit too late for one group, and that's the sharper lesson.** Executions
created by the ungated build have the Activity in their history and no marker, so they resolve
the same way a pre-Phase-6 run does — and replay then omits an Activity the history demands.
`GetVersion` can't tell "predates the change" from "ran the change before it was gated", because
the marker is the only signal and neither has one. Gating is still the right trade, but nothing
written later can repair those histories. **Gate a command-changing edit in the same commit that
introduces it.**

It also makes the migration observable, because `GetVersion` upserts a built-in search
attribute:

```sh
# how many customers have picked up the change?
temporal workflow count --query "TemporalChangeVersion = 'tier-notifications-1'"
```

One trap, since it cost real time: `worker.ReplayWorkflowHistory` runs the workflow under a fake
ID (`"ReplayId"`) unless you pass `OriginalExecution`. Any workflow that reads its own ID — ours
validates against it — then fails with a nondeterminism error naming a completely innocent line.
It reads exactly like a versioning bug. `internal/rewards/replay_test.go` pins that so nobody
simplifies the workaround away.

## Seeding a demo

```sh
make seed            # needs make up + make worker + make api
make reset && make seed   # for a clean slate
```

Nine customers covering the cases a demo needs: every tier, an empty timeline, a deactivated
customer, and one just under the points cap. It drives the HTTP API rather than the Temporal
client, so seeding exercises the path a user takes — rollover retries and error mapping included.

**Read-then-create, and idempotent.** It looks each customer up first, enrolls only the missing
ones, and reports any existing customer whose balance disagrees with the dataset rather than
trying to repair it — points only go up, so a balance that is too high cannot be corrected.

There is deliberately no "replace" flag. Deactivation is soft, so enrolling an existing customer
*reactivates* them with their points intact; a flag that deactivated and re-enrolled to get a
clean slate would stack a second set of adds on the balance it kept. That is not hypothetical —
it is what `FRESH=1` started doing when soft deactivation landed, leaving `ada` at 1280 points
instead of 640. The only real clean slate is deleting the executions: `make reset`.

`capped` is the interesting one: 100 adds to land just under the points cap, which makes handler
rejections reachable and produces ~33 generations. Follow it with `make reap WF=customer-capped`
and `make audit ID=capped` for a truncated audit log.

## Running more than one stack

Ports and container names all come from the env file, and `COMPOSE_PROJECT_NAME` isolates
containers, networks, and volumes:

```sh
cp .env.example .env.beta     # set STACK_NAME=beta and bump every *_PORT
make up ENV=.env.beta
```

Elasticsearch is the expensive part (~500–700 MB per stack even tuned down — see §7.3 of the
plan). If you only need isolated workflows rather than isolated infrastructure, a second
namespace on one stack is much cheaper.

## Poking at it

```sh
make tools            # shell in the server container, `temporal` CLI on PATH
make psql             # interactive psql into Temporal persistence
make es               # Elasticsearch visibility index summary
make inspect          # canned §8 queries into both stores
make write-trace ID=inspect AMOUNT=10
make logs SVC=temporal
```

How Temporal actually uses Postgres (persistence) and Elasticsearch (visibility) — including
an end-to-end write trace — is documented in [docs/DATASTORES.md](docs/DATASTORES.md).
`make psql Q=history-blob ID=inspect` / `make es Q=mapping` run the same canned queries.

## Two things worth knowing up front

**Retention is 1 hour, not the 20 minutes the plan wanted.** Temporal enforces a 1 h minimum
namespace retention, and the dynamic config key that would lower it
(`system.namespaceMinRetentionLocal`) does not exist in any released server — 1.29.7 logs
`unregistered key` and ignores it. Since the point of a short retention was to make
audit-log truncation quick to observe, `make reap` deletes closed executions on demand
instead:

```sh
make reap                      # every closed execution
make reap WF=customer-abc123   # just that customer's closed runs
```

Running executions are filtered out deliberately — `workflow delete` *terminates* a running
execution before deleting it, so an unfiltered reap would destroy active customers rather
than just their old continue-as-new generations.

**Visibility is asynchronous.** A newly created or updated workflow does not appear in
`ListWorkflows` immediately. The stack tunes this down to roughly 200–300 ms (server-side
flush interval plus the Elasticsearch index refresh interval, which Temporal's index template
leaves at the 1 s default), but it is never zero. Anything needing read-after-write should use
Query or Describe, which read persistence rather than the visibility store.

**You cannot sort a workflow list.** `ORDER BY` is rejected by the server — not only for our
custom attributes but for built-ins like `StartTime` too, even with Elasticsearch visibility
and custom attributes otherwise working:

```sh
# filtering: fine
temporal workflow list --query "RewardsLevel = 'gold' AND ExecutionStatus = 'Running'"
temporal workflow list --query "RewardsPoints >= 500"

# sorting: ORDER BY clause is not supported
temporal workflow list --query "RewardsLevel = 'gold' ORDER BY RewardsPoints DESC"
```

Filter server-side, sort client-side — and note that sorting a *page* of a paginated list
sorts the wrong thing. Temporal's visibility store is a filter index, not a reporting
database. If a rewards program needed leaderboards, that is the point where a read model
projected out of Temporal starts earning its keep.

## Verifying the platform assumptions

```sh
make verify-config
```

Checks the three behaviours the design depends on: that the 1 h retention floor is still
enforced, that every key in `deploy/dynamicconfig/dev.yaml` is actually registered by the
running server (an unregistered key is silently ignored after one startup warning), and that
on-demand deletion works. Worth re-running after any server upgrade.

## Layout

```
cmd/worker/                   the worker process
cmd/api/                      the HTTP API
cmd/seed/                     demo data, via the API rather than around it
internal/rewards/
  state.go                    CustomerState, tier thresholds, derived Level()
  workflow.go                 CustomerRewardsWorkflow, addPoints, deactivate, reactivate, getStatus
  searchattr.go               typed search attribute keys
  notify.go                   the notification contract the audit crawl decodes
  notifier.go                 promotion detection and delivery, run from the main loop
  activity.go                 NotifyCustomer -- the only side effect in the system
  workflow_test.go            unit tests (no Docker required)
  replay_test.go              deploy rehearsal against recorded histories
  testdata/                   real histories, including pre-Phase-6 ones
internal/httpapi/
  server.go                   enroll/re-enroll, detail, add points, deactivate, list
  audit.go                    the Event History crawl and truncation detection
  classify.go                 error shapes, measured against a real server
  dto.go                      the wire contract, frozen ahead of the endpoints
  testdata/                   real run histories, for the crawl's golden tests
deploy/
  docker-compose.yml          Postgres + Elasticsearch + Temporal + UI
  dynamicconfig/dev.yaml      retention jitter, visibility flush interval
  bootstrap.sh                namespace + search attributes (idempotent)
  reap.sh                     force-delete closed executions
  inspect/verify-config.sh    platform assumption checks
docs/PLAN.md                  the design
.env.example                  ports, versions, tuning
Makefile
```
