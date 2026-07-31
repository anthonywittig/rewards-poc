# rewards-poc

Demonstrating **Temporal as the system of record** for a customer rewards program.

There is no application database for rewards state. A customer's points, tier, enrollment
date, and history of point-earning events live entirely in a Temporal Workflow Execution and
its Event History. See [docs/PLAN.md](docs/PLAN.md) for the full design.

**Status: Phase 2.** The workflow runs, continues-as-new every 3 point-adds, and is drivable
end to end from the `temporal` CLI. No HTTP API or UI yet — those are Phases 3 and 8.

## Quick start

Requires Docker with Compose v2. Roughly 1.5 GB of images and ~1 GB of RAM. Running the worker
also needs Go — 1.25.4 or newer, which is the Temporal Go SDK's own floor, not ours (on an
older Go the default `GOTOOLCHAIN=auto` will fetch it for you).

```sh
cp .env.example .env
make up
```

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
make deactivate ID=c-001                          # cancel, not terminate
```

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

Three is artificially low so the rollover is easy to watch. Production code should let the
server decide from actual history size:

```sh
make worker EARNS_PER_RUN=0   # defer to GetContinueAsNewSuggested()
```

Under that setting the same seven adds leave `generation` at 0 — seven adds are nowhere near
enough history for the server to suggest rolling, which is precisely why a fixed count is the
demonstrable one and the server's judgement is the correct one.

**Changing this value under running workflows causes non-determinism errors** — a run whose
history records a roll after 3 adds will not produce that command at that point when replayed
under a different threshold. In dev, terminate existing workflows after changing it.

One customer is one long-lived Workflow Execution with the ID `customer-<id>`, which is why
none of these commands need a lookup table. Points arrive as Updates, status is a Query, and
deactivation is a cancellation.

**Re-enrolling a deactivated customer starts them over at zero.** The workflow ID is free once
the execution closes, so enrollment succeeds — but it is a genuinely new enrollment, not a
restoration. That is a decision, not an oversight; see §3.6 of the plan.

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
the denial is permanently recorded and will show up in the audit log built in Phase 5. Which
is the point: *"why didn't this customer reach platinum?"* has an answer, while *"someone's
integration sent a negative number 4,000 times"* does not clutter the record.

The rule of thumb, and where each rejection lives in the code:

| | Where | Writes history? | Example |
|---|---|---|---|
| Facts about the **request** | Update validator | No | negative amount, over per-txn max, missing reason |
| Facts about the **customer** | Update handler | Yes | would exceed the points cap |

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
make psql             # psql into the Temporal persistence database
make es               # Elasticsearch visibility index summary
make logs SVC=temporal
```

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
internal/rewards/
  state.go                    CustomerState, tier thresholds, derived Level()
  workflow.go                 CustomerRewardsWorkflow, addPoints, getStatus
  searchattr.go               typed search attribute keys
  workflow_test.go            unit tests (no Docker required)
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
