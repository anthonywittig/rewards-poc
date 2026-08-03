# rewards-poc

A minimal demonstration of the **Entity Workflow** pattern: one long-lived
Temporal workflow per business entity, used as the entity's system of record.

Here the entity is a customer in a rewards program. There is no application
database: a customer's points, tier, enrollment date and membership live
entirely in one Workflow Execution with the ID `customer-<id>`. Points arrive
as **Updates**, status is a **Query**, the customer list is a **visibility
query** over search attributes, and the audit trail is the **Event History**
Temporal already keeps. Every few point-adds the workflow **continues as new**
so its history stays bounded.

The whole thing is the workflow (`internal/rewards/workflows/workflow.go`),
the plain-function rules it applies (`internal/rewards/`), and a worker.
It is driven from the `temporal` CLI via the Makefile — there is no
application server or UI to read past.

## Quick start

Prerequisites: **Docker** with Compose v2. **Go** 1.25.4+ (the Temporal Go
SDK's floor; older Go fetches it via `GOTOOLCHAIN=auto`) is only needed for
`make test` and `make workflowcheck`.

```sh
# Postgres, Elasticsearch, Temporal + its UI, and the workflow worker.
make up
```

| | |
|---|---|
| Temporal UI | <http://localhost:8080> |
| Temporal gRPC | `localhost:7233`, namespace `rewards` |

`make logs SVC=worker` tails the worker; `make worker` rebuilds and restarts
it after a code change. `make down` stops the stack and keeps the data;
`make destroy` deletes the volumes too.

## Driving it

```sh
make enroll ID=c-001 NAME="Ada Lovelace"
make status ID=c-001
make add    ID=c-001 AMOUNT=499 REASON=purchase
make add    ID=c-001 AMOUNT=1   REASON=purchase   # -> 500, promoted to gold
make deactivate ID=c-001                          # soft leave; the workflow keeps running
make reactivate ID=c-001                          # rejoin, balance intact
make list                                         # all customers, from visibility
make history ID=c-001                             # the Event History = the audit trail
```

Everything lands in one workflow execution — open `customer-c-001` in the
Temporal UI and watch the events accumulate as you go.

## Things worth seeing

**Continue-as-new.** Every 3 successful point-adds the workflow ends its run
and starts a fresh one carrying state forward:

```sh
make enroll ID=roll NAME="Rolly Poly"
for i in 1 2 3 4 5 6 7; do make add ID=roll AMOUNT=100 REASON="add $i"; done
make status ID=roll     # generation 2, points 700
```

The balance accumulates across the boundary while `generation` ticks up, and
the current run's history stays a dozen events long instead of growing
forever. Three is a demo number chosen to be watchable — production should ask
`workflow.GetInfo(ctx).GetContinueAsNewSuggested()` instead. Changing the
constant breaks running workflows (see `EarnsPerRun`).

**The validator/handler split.** Both of these fail identically from the
caller's side, but only one leaves a trace:

```sh
make add ID=c-001 AMOUNT=-50 REASON=oops    # validator: writes no history at all
make deactivate ID=c-001
make add ID=c-001 AMOUNT=10 REASON=too-late # handler: the rejection is recorded
```

Count events either side with `make history ID=c-001`: a validator rejection
adds none — a client stuck retrying `amount: -1` cannot grow history by a
single event — while a rejection that depends on the customer's *state* is
permanently recorded. Facts about the **request** belong in the validator,
facts about the **customer** in the handler.

**Listing entities is a visibility query.** The workflow upserts search
attributes (`RewardsLevel`, `RewardsPoints`, `RewardsActive`, ...), so finding
customers needs no lookup table — and the same queries work as typed in the
Temporal UI:

```sh
make list Q="RewardsLevel = 'gold'"
make list Q="RewardsPoints >= 500"
make list Q="RewardsActive = false"
```

**The replay test is the one that matters.**

```sh
go test ./internal/rewards/workflows/ -run TestReplay
```

A customer's workflow outlives deploys, so today's code gets replayed against
histories recorded weeks ago and the commands must match event for event.
`testdata/run-enrollment.json` is a real recorded history; an edit that
changes what the workflow emits fails this test before it wedges every
customer with an open run in production. The production-grade fix for such an
edit is `workflow.GetVersion`, which this POC deliberately omits: executions
here can simply be reset (`make reset`).

**The determinism check runs before the replay test can save you.**

```sh
make workflowcheck
```

The Go SDK has no workflow sandbox: `time.Now()` in workflow code compiles,
passes vet and the unit tests — then wedges a customer on replay, weeks
later. `workflowcheck` statically flags anything reachable from workflow code
that is non-deterministic.

**No Activities, deliberately.** Nothing in the rewards program touches the
outside world — the workflow is a pure state machine and Temporal is its
store, which is rather the point. A real system would notify customers on
promotion; that is an Activity, and it belongs in a sibling package the
workflow schedules by name, never by import, because that package boundary is
the only structural guard the Go SDK has.

## Behaviour to expect

- **Points only go up.** No spending, redemption or expiry, so tiers never
  demote and the balance is also the lifetime total.
- **Leaving is soft.** Deactivation sets a flag via Update; the execution
  stays Running, so re-enrolling restores balance and history intact.
  Membership lives in the `RewardsActive` search attribute, not in
  `ExecutionStatus`.
- **Visibility is asynchronous.** A new or updated workflow appears in
  `make list` after ~200–300 ms, never instantly. Read-after-write needs the
  Query, which reads persistence.
- **You cannot sort a workflow list.** `ORDER BY` is rejected by the server.
  Filter server-side, sort client-side.
- **Retention is 1 hour** after a run closes — Temporal's enforced minimum —
  so old generations' histories eventually age out; workflow *state* carried
  through continue-as-new does not.

## Troubleshooting

**The workflow seems to ignore a code change?** The worker is still running
the old image — editing Go code does nothing until `make worker` rebuilds it.
Stale *workflows* fail loudly on replay; stale *workers* succeed quietly with
the wrong logic.

**`BadSearchAttributes: search attribute ... is not defined` right after
`make up`?** The server caches search attribute definitions for about a
minute. `dev.yaml` sets `system.forceSearchAttributesCacheRefreshOnRead` to
read through that cache, so if you see this, check that the dynamic config is
mounted.

**Changed workflow code and existing runs now misbehave?** Constants like
`EarnsPerRun` are baked into recorded history. In dev, `make reset` and start
over.

## Layout

```
cmd/worker/                   the worker process
internal/rewards/             the domain: types and rules, no Temporal orchestration
  state.go                    CustomerState, tier thresholds, EarnsPerRun
  contract.go                 the Update/Query contract every caller speaks
  enrollment.go               what makes a starting payload valid
  searchattr.go               typed search attribute keys
internal/rewards/workflows/   the workflow layer
  workflow.go                 CustomerRewardsWorkflow: Updates, Query, continue-as-new
  workflow_test.go            unit tests (no Docker required)
  replay_test.go              deploy rehearsal against a recorded history
  testdata/                   a real recorded history
deploy/
  docker-compose.yml          the whole stack, and every setting it has
  Dockerfile                  the worker image
  dynamicconfig/dev.yaml      visibility flush interval, search attribute cache
  bootstrap.sh                namespace + search attributes (idempotent)
  reset.sh                    delete every customer workflow (make reset)
Makefile                      `make help` lists every target
```
