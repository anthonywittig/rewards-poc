# rewards-poc

A POC of the **Entity Workflow** pattern: **Temporal as the system of record**
for a customer rewards program. Written for developers who already know
Temporal basics and want to see what using it as the *data store* looks like.

There is no application database. A customer's points, tier, enrollment date,
and history of point-earning events live entirely in a Workflow Execution and
its Event History. One customer is one long-lived workflow with the ID
`customer-<id>`:

- **points arrive as Updates** (`addPoints`, with a validator),
- **current state is a Query** (`getStatus`),
- **the customer list is a visibility query** over custom search attributes,
- **the audit log is reconstructed by crawling Event History**,
- **the workflow continues-as-new** every few updates to keep history bounded.

The API holds a Temporal client and nothing else — no database, no cache, no
ORM.

## Quick start

Prerequisites: **Docker** with Compose v2, and nothing else — every process
runs in the stack. **Go** 1.25.4+ is only needed for `make test`, and **Node**
only if you want to run `npm` against `web/` yourself.

```sh
# Postgres, Elasticsearch, Temporal + its UI, the worker, the HTTP API, the
# demo customers, and the React UI. A couple of minutes the first time; the UI
# starts last and is not waited on (`make logs SVC=web` shows its progress).
make up
```

| | |
|---|---|
| React UI | <http://localhost:5173> |
| HTTP API | <http://localhost:8081/api/customers> |
| Temporal UI | <http://localhost:8080> |
| Temporal gRPC | `localhost:7233`, namespace `rewards` |

The worker, API and Vite dev server are all Compose services: `make logs
SVC=worker` (or `api`, or `web`) tails them, `make worker` / `make api`
rebuild and restart them after a code change, and `web/` is bind-mounted so UI
edits hot-reload. `make test` needs no Docker. `make down` stops the stack;
`make destroy` deletes the volumes too; `make help` lists everything.

## Driving it from the CLI

The whole workflow is usable with no API and no UI — these targets go straight
to the `temporal` CLI inside the server container.

```sh
make enroll ID=c-001 NAME="Ada Lovelace"
make status ID=c-001
make add    ID=c-001 AMOUNT=499 REASON=purchase
make add    ID=c-001 AMOUNT=1   REASON=purchase   # -> 500, promoted to gold
make audit  ID=c-001                              # the timeline, crawled out of Event History
make deactivate ID=c-001                          # one-way: records the leave, completes the workflow
```

## The HTTP API

```sh
# No customerId: the server derives one from the name (here, ada-lovelace).
# The same name derives the same ID, so a second signup is a 409.
curl -XPOST localhost:8081/api/customers -d '{"name":"Ada Lovelace"}'

curl localhost:8081/api/customers/c-001
curl -XPOST localhost:8081/api/customers/c-001/points -d '{"amount":500,"reason":"purchase"}'
curl -XDELETE localhost:8081/api/customers/c-001
curl localhost:8081/api/customers/c-001/audit
```

`GET /api/customers` is a `ListWorkflow` plus a `CountWorkflow` — no lookup
table. Filtering is structured, and the server builds the visibility query
from the params; the response echoes the query it built, **pasteable into the
Temporal UI unchanged**:

```sh
curl -sG localhost:8081/api/customers --data-urlencode "tier=gold"
curl -sG localhost:8081/api/customers --data-urlencode "status=deactivated"
curl -sG localhost:8081/api/customers --data-urlencode "name=ada"   # word-prefix match
```

Failures are `{"error":{"code":"...","message":"..."}}` with a stable code —
notably `worker_unavailable` (503) when nothing is polling the task queue,
`rejected` (422) when the workflow refused a request, and `deactivated` (409)
for anything touching a departed customer.

## Things worth seeing

**Continue-as-new.** Every 3 successful point-adds the workflow ends its run
and starts a fresh one carrying state forward:

```sh
make enroll ID=roll NAME="Rolly Poly"
for i in 1 2 3 4 5 6 7; do make add ID=roll AMOUNT=100 REASON="add $i"; done
make status ID=roll     # generation 2, points 700
```

The balance accumulates across the boundary while `generation` ticks up, and
each run's history stays small. Three is a demo number chosen to be watchable;
production should ask `workflow.GetInfo(ctx).GetContinueAsNewSuggested()`.

**The audit log is the Event History.** Nothing stores a customer's point-add
history; `make audit ID=c-001` walks back through the run chain and reads the
events Temporal recorded because it had to in order to run the workflow at
all. Closed runs are deleted after retention (1 hour here, Temporal's
minimum), so the response reports truncation — "showing 3 of 21" — rather than
quietly showing less.

**The validator/handler split.** Both of these fail identically from the
caller's side, but only one leaves a trace:

```sh
make add ID=c-001 AMOUNT=-50 REASON=oops        # validator: writes no history at all
make add ID=capped AMOUNT=100 REASON="over cap" # handler: recorded, shows as points_rejected
```

`capped` comes from `make seed`, parked at 4,960 points so that any add over
40 breaches the 5,000 cap. A validator rejection writes no events — a client
stuck retrying `amount: -1` cannot grow history — while a rejection that
depends on the customer's accumulated state is permanently recorded. Facts
about the *request* belong in the validator, facts about the *customer* in the
handler.

**Deactivation completes the workflow.** Leaving the program is one-way: the
`deactivate` Update sets the flag, the run drains its handlers, and the
workflow completes normally with the balance frozen in its final state. The
detail page, list and audit log keep answering for a departed customer (Query
and Describe work on closed runs until retention reaps them), and the enroll
endpoint refuses to reuse the ID — `ALLOW_DUPLICATE_FAILED_ONLY` retires a
completed execution's ID while still letting a *failed* enrollment be retried.

**The replay test.** A customer's workflow outlives deploys, so today's code
gets replayed against histories recorded weeks ago and the commands must match
event for event:

```sh
go test ./internal/rewards/workflows/ -run TestReplay
```

`testdata/run-enrollment.json` is a real recorded history; an edit that
changes what the workflow emits fails this test before it wedges every open
run in production. (The production-grade fix for such an edit is
`workflow.GetVersion`, which this POC omits — executions here can simply be
reset.)

**The determinism check.** The Go SDK has no workflow sandbox: `time.Now()` in
workflow code compiles and passes tests, then wedges a customer on replay.
`make workflowcheck` statically flags anything reachable from workflow code
that is non-deterministic.

**No Activities, deliberately.** Nothing in the rewards program touches the
outside world — the workflow is a pure state machine and Temporal is its
store, which is the argument of the POC. A real system would notify customers
on promotion, and that is what an Activity is for.

## Behaviour to expect

- **Points only go up.** No spending or expiry, so tiers never demote and
  `Points` is also the lifetime total.
- **Leaving is one-way.** Deactivation records the leave and completes the
  workflow; there is no reactivation, and the customer's ID stays retired
  until their history is reaped. Membership lives in the `RewardsActive`
  search attribute, which the completed final run keeps.
- **Visibility is asynchronous.** A new or updated workflow appears in the
  list after ~200–300 ms, never instantly. Read-after-write goes through
  Query or Describe instead.
- **You cannot sort a workflow list.** Temporal rejects `ORDER BY`, so the
  list endpoint returns at most five rows, reports how many matched, and
  pushes you to filter. Leaderboards are where a separate read model would
  start earning its keep.

## Troubleshooting

**A code change seems ignored?** The worker runs in the stack, so editing Go
code does nothing until `make worker` rebuilds it. Stale workflows fail loudly
on replay; stale workers succeed quietly with the old logic.

**503 `worker_unavailable`?** Nothing is polling the task queue — check
`make ps`, run `make worker`.

**Changed workflow code and existing runs misbehave?** Constants like
`EarnsPerRun` are baked into recorded history. In dev, `make reset` and start
over.

## Layout

```
cmd/worker/                   the worker process
cmd/api/                      the HTTP API
cmd/seed/                     demo data, via the API
internal/rewards/             the domain: types and rules, no workflow code
  state.go                    CustomerState, tier thresholds, derived Level()
  contract.go                 the Update/Query contract every caller speaks
  enrollment.go               what makes a starting payload valid
  customerid.go               customer IDs derived from names
  searchattr.go               typed search attribute keys
  workflows/
    workflow.go               CustomerRewardsWorkflow: updates, query, continue-as-new
    workflow_test.go          unit tests (no Docker required)
    replay_test.go            deploy rehearsal against a recorded history
internal/httpapi/
  server.go                   enroll, detail, add points, deactivate, list
  filter.go                   structured list params -> visibility query clauses
  audit.go                    the Event History crawl and truncation detection
  classify.go                 Temporal error classification
  errors.go                   stable error codes and their HTTP mapping
  dto.go                      the wire contract
web/                          the React UI
deploy/
  docker-compose.yml          the whole stack, and every setting it has
  Dockerfile                  the worker, api and seed images
  bootstrap.sh                namespace + search attributes (idempotent)
  reset.sh                    delete every customer workflow (make reset)
Makefile
```
