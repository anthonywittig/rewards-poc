# rewards-poc

Demonstrating **Temporal as the system of record** for a customer rewards program.

There is no application database for rewards state. A customer's points, tier, enrollment date,
and history of point-earning events live entirely in a Temporal Workflow Execution and its Event
History. One customer is one long-lived workflow with the ID `customer-<id>`: points arrive as
Updates, status is a Query, the customer list is a visibility query, and the audit log is
reconstructed by crawling Event History. The API holds a Temporal client and **nothing else** —
no database, no cache, no ORM.

**Complete.** The workflow runs, continues-as-new every 3 point-adds, and is drivable from the
`temporal` CLI, over HTTP, or through the React UI.

## Quick start

Prerequisites: **Docker** with Compose v2 (~1.7 GB of images, ~1 GB of RAM), and nothing else —
every process runs in the stack, Go compiled into its images and the UI served from the `node`
one. **Go** 1.25.4 or newer (the Temporal Go SDK's floor, not ours — on an older Go the default
`GOTOOLCHAIN=auto` fetches it) is only for `make test` and `make workflowcheck`, and **Node** 20+
only if you want to run `npm` against `web/` yourself.

There is no env file to copy: every setting is written out literally in `deploy/docker-compose.yml`.
Change a port or an image version by editing it there.

```sh
# The whole stack: Postgres, Elasticsearch, Temporal + its UI, the worker, the
# HTTP API, the demo customers, and the React UI. Takes a couple of minutes the
# first time (it compiles the Go services into their images); the UI starts
# last and is not waited on -- `make logs SVC=web` shows its progress.
make up
```

That's everything up. Where it all is:

| | |
|---|---|
| React UI | <http://localhost:5173> |
| HTTP API | <http://localhost:8081/api/customers> |
| Temporal UI | <http://localhost:8080> |
| Temporal gRPC | `localhost:7233`, namespace `rewards` |

Nothing here has a terminal of its own — the worker, the API and the Vite dev server are all
Compose services, so `make logs SVC=worker` (or `api`, or `web`) tails them, and
`make worker` / `make api` rebuild and restart them after a code change. The UI needs no restart:
`web/` is bind-mounted, so edits hot-reload. The seed is a Compose one-shot off the same image as
the worker and API.

`make test` runs the Go unit tests and needs neither Docker nor a running server. `make down`
stops the stack and keeps the data; `make destroy` deletes the volumes too.

## Driving it from the CLI

The whole workflow is usable with no API and no UI — these targets go straight to the `temporal`
CLI inside the server container.

```sh
make enroll ID=c-001 NAME="Ada Lovelace"
make status ID=c-001
make add    ID=c-001 AMOUNT=499 REASON=purchase
make add    ID=c-001 AMOUNT=1   REASON=purchase   # -> 500, promoted to gold
make deactivate ID=c-001                          # leave for good; the workflow completes
make audit  ID=c-001                              # the timeline, crawled out of Event History
```

Deactivation is one-way: it completes the customer's workflow, freezing the balance for as
long as the closed run survives retention. There is no reactivate — enrolling the same name
again is refused, because the ID derived from it still belongs to the departed customer.

## Commands

`make help` lists every target. The ones you'll actually use:

| | |
|---|---|
| `make up` / `down` / `destroy` | start + bootstrap + seed / stop / stop and delete volumes |
| `make ps` / `logs SVC=temporal` | stack status / tail one service |
| `make worker` / `worker-stop` | rebuild + restart / stop the worker service |
| `make api` | rebuild + restart the HTTP API on `:8081` |
| `make web` | start or restart the UI on `:5173` |
| `make web-check` | typecheck and production-build the UI (the dev server doesn't) |
| `make test` | Go unit tests, no Docker needed |
| `make workflowcheck` | static determinism check on workflow code, no Docker needed |
| `make seed` / `reset` | demo customers, also run by `make up` (idempotent) / delete every customer workflow |
| `make reap [WF=customer-x]` | delete closed runs now, to force audit-log truncation |
| `make tools` / `psql` / `es` / `inspect` | shell with the `temporal` CLI / datastore access |

## The HTTP API

```sh
# No customerId: the server derives one from the name (here, ada-lovelace) and
# returns it. Signing up twice under one name is the same customer, so the
# second attempt is a 409.
curl -XPOST localhost:8081/api/customers \
  -d '{"name":"Ada Lovelace"}'

# Sending one picks the ID yourself, whatever the name says.
curl -XPOST localhost:8081/api/customers \
  -d '{"customerId":"c-001","name":"Ada Lovelace"}'

curl localhost:8081/api/customers/c-001
curl -XPOST localhost:8081/api/customers/c-001/points -d '{"amount":500,"reason":"purchase"}'
curl -XDELETE localhost:8081/api/customers/c-001
curl localhost:8081/api/customers/c-001/audit
```

`GET /api/customers` is a `ListWorkflow` plus a `CountWorkflow` and nothing else — no lookup
table, no local index. `?q=` is passed to Temporal essentially as typed, so **the same query
works unchanged in the Temporal UI**:

```sh
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsLevel = 'gold'"
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsPoints >= 500"
curl -sG localhost:8081/api/customers --data-urlencode "q=CustomerName = 'Ada'"    # Text, partial
curl -sG localhost:8081/api/customers --data-urlencode "q=RewardsActive = false"   # deactivated
```

A rejected query comes back as a 400 carrying Temporal's own diagnostics. `ORDER BY` is the one
exception, caught before the query is sent so you get an explanation instead of a bare syntax
error. Because there's no ordering there's no meaningful pagination either: the list returns at
most five rows, reports how many matched, and pushes you to filter — which is what the
visibility store is good at.

Every failure is `{"error":{"code":"...","message":"..."}}` with a stable code:

| | Code | When |
|---|---|---|
| 400 | `invalid_request` | malformed body, a missing `name`, a `name` with no letters or digits to derive an ID from, a `customerId` with whitespace or a slash in it, unknown JSON field |
| 404 | `not_found` | no such customer, or their history was reaped |
| 409 | `already_exists` | enrolling a customer who is already active |
| 409 | `deactivated` | adding points to, or re-enrolling, a customer who has left — deactivation is one-way |
| 409 | `rollover_race` | the workflow rolled over twice while applying one request |
| 422 | `rejected` | the workflow refused it |
| 503 | `worker_unavailable` | nothing is polling the task queue — or, less often, Temporal itself is slow or unreachable (this is the contract's only 503) |

The 503 is the one you'll meet most, because the worker is down more often than anything else in
development.

The UI reaches the API through Vite's proxy rather than a cross-origin base URL — the Go API
deliberately sends no CORS headers.

## Things worth seeing

Each of these takes a couple of commands against a running stack.

**Continue-as-new.** Every 3 successful point-adds the workflow ends its Run and starts a fresh
one carrying state forward.

```sh
make enroll ID=roll NAME="Rolly Poly"
for i in 1 2 3 4 5 6 7; do make add ID=roll AMOUNT=100 REASON="add $i"; done
make status ID=roll     # generation 2, points 700
```

The balance accumulates across the boundary while `generation` ticks up, and the current run's
history stays under a dozen events against the ~47 the same seven adds accumulate without
rolling. Three is a demo number chosen to be watchable, not a defensible rule — the real limits
are 50k events and 50 MB per run, and production should ask
`workflow.GetInfo(ctx).GetContinueAsNewSuggested()` instead. Changing the constant breaks
running workflows.

**The audit log is the Event History.** Nothing stores a customer's point-add history; `make
audit ID=c-001` walks back through the run chain and reads the events Temporal recorded because
it had to in order to run the workflow at all. It needs no worker (it only talks to the server,
so it answers in 10 ms while the detail page 503s) and it's cheap — 34 runs and 100 point-adds
crawl end to end in ~125 ms. Closed runs get reaped, so the response reports truncation rather
than quietly showing less:

```sh
make reap WF=customer-capped    # deletes the closed generations, keeps the running one
make audit ID=capped            # truncated=True shown=1 lifetime=100 runsWalked=1
```

Demonstrating the limitation is worth more than hiding it.

**The validator/handler split.** Both of these fail identically from the caller's side, but only
one leaves a trace:

```sh
make add ID=c-001 AMOUNT=-50 REASON=oops       # validator: writes no history at all
make add ID=capped AMOUNT=41 REASON="over cap" # handler: recorded, shows as points_rejected
```

`capped` comes from `make seed`, parked at 99,960 points precisely so that any add over 40
breaches the 100,000 cap. Count history events either side in the Temporal UI: a validator
rejection adds none, so a client stuck retrying `amount: -1` cannot grow history by a single
event, while a rejection that depends on the customer's accumulated state is permanently
recorded. Facts about the *request* belong in the validator, facts about the *customer* in
the handler.

**The replay test is the one that matters.**

```sh
go test ./internal/rewards/workflows/ -run TestReplay
```

A customer's workflow outlives deploys, so today's code gets replayed against histories recorded
weeks ago and the commands must match event for event.
`internal/rewards/workflows/testdata/run-enrollment.json` is a real recorded history; an edit
that changes what the workflow emits — adding an Activity, reordering commands — fails this test
before it wedges every customer with an open run in production. The production-grade fix for such
an edit is `workflow.GetVersion`, which this POC deliberately omits: executions here can simply
be reset, and the gate's markers and replay fixtures were most of what made the workflow hard to
read.

**The determinism check runs before the replay test can save you.**

```sh
make workflowcheck
```

The Go SDK has no workflow sandbox. `time.Now()` in workflow code compiles, passes `go vet`, and
passes the unit tests — then wedges a customer on replay, weeks later, in production.
`workflowcheck` walks the call graph from every function taking a `workflow.Context` and flags
anything reaching a non-deterministic call, transitively: a `time.Now()` anywhere the workflow
can reach is reported with the chain that reaches it.
Replay tests catch this too, but only for the paths a recorded history happens to cover.

**No Activities, deliberately.** Nothing in the rewards program touches the outside world:
points, tier, membership and the audit trail are all workflow state and Event History, which is
rather the argument of the POC — the workflow is a pure state machine and Temporal is its store.
A real system would notify customers on promotion, and that is what an Activity is for: it would
live in a sibling `internal/rewards/activities` package the workflow schedules **by name**, never
by import. The Go SDK has no workflow sandbox — nothing at runtime stops workflow code from
calling a provider SDK directly and silently breaking determinism — so that package boundary is
the only structural guard there is.

## Behaviour to expect

- **Points only go up.** No spending, redemption, expiry, or adjustment, and none is planned. So
  tiers never demote either, and the single `Points` field is also the lifetime total.
- **Leaving is one-way.** Deactivation is an Update that sets a flag and then completes the
  workflow — no reactivation, no restart under the same name. The final run keeps
  `RewardsActive: false` in visibility, so departed customers stay listable until their
  closed run is reaped; membership lives in that attribute rather than in `ExecutionStatus`.
- **Cancellation is not part of the model.** Nothing cancels a customer's workflow and the code
  does not handle it. `temporal workflow cancel` closes the execution without upserting
  `RewardsActive`, leaving a customer the list still calls active and the detail page calls
  deactivated.
- **Visibility is asynchronous.** A new or updated workflow doesn't appear in `ListWorkflows`
  immediately — tuned down to ~200–300 ms here, but never zero. Anything needing read-after-write
  should use Query or Describe, which read persistence.
- **You cannot sort a workflow list.** `ORDER BY` is rejected by the server, for built-ins like
  `StartTime` as much as for our own attributes. Filter server-side, sort client-side — and note
  that sorting a *page* sorts the wrong thing. Leaderboards are where a read model projected out
  of Temporal starts earning its keep.
- **Retention is 1 hour**, Temporal's enforced minimum; the key that would lower it doesn't exist
  in any released server. `make reap` deletes closed executions on demand instead. It spares
  running executions on purpose — `workflow delete` *terminates* a running execution first, so an
  unfiltered reap would destroy active customers rather than just their old generations.

## Troubleshooting

**If the workflow seems to ignore a code change, the worker is still running the old image.**
The worker and the API both run in the stack, built from `deploy/Dockerfile`, so editing Go code
does nothing until the image is rebuilt:

```sh
make worker            # rebuild from the current code and restart the container
make logs SVC=worker   # tail it -- the startup line names the task queue and namespace
make api               # the same for the API
make logs SVC=api
```

Stale *workflows* fail loudly on replay; stale *workers* succeed quietly with the wrong logic, so
rebuild before concluding anything about workflow behaviour.

**`BadSearchAttributes: search attribute CustomerId is not defined` in the worker log, right
after a fresh `make up`?** The server caches search attribute definitions for about a minute
after `bootstrap.sh` registers them. `dev.yaml` sets
`system.forceSearchAttributesCacheRefreshOnRead` to read through the cache, which is why you
should not see it; if you do, check that the dynamic config is mounted.

**A 503 `worker_unavailable` usually means nothing is polling the task queue** — check
`make ps` and run `make worker` if it isn't up. (The same code also covers a slow or unreachable
Temporal, because it is the contract's only 503 — so if the worker is running, look at the rest
of `make ps` before restarting it.)

**Changed the workflow code and existing runs now misbehave?** Constants like `EarnsPerRun` are
baked into recorded history. In dev, `make reset` and start over.

## Layout

```
cmd/worker/                   the worker process
cmd/api/                      the HTTP API
cmd/seed/                     demo data, via the API rather than around it
internal/rewards/             the domain: types and rules, no Temporal orchestration
  state.go                    CustomerState, tier thresholds, derived Level()
  contract.go                 the Update/Query contract every caller speaks
  enrollment.go               what makes a starting payload valid
  searchattr.go               typed search attribute keys
  level_test.go               tier derivation, no test environment needed
  tiers_test.go               the tier ladder's ordering invariant
  workflows/                  the workflow layer
    workflow.go               CustomerRewardsWorkflow, addPoints, deactivate, getStatus
    workflow_test.go          unit tests (no Docker required)
    replay_test.go            deploy rehearsal against a recorded history
    testdata/                 a real recorded history
internal/httpapi/
  server.go                   enroll, detail, add points, deactivate, list
  audit.go                    the Event History crawl and truncation detection
  classify.go                 Temporal error classification, measured against a real server
  errors.go                   the stable error codes and their HTTP mapping
  dto.go                      the wire contract, frozen ahead of the endpoints
  testdata/                   real run histories, for the crawl's golden tests
web/                          the React UI
deploy/
  docker-compose.yml          the whole stack, and every setting it has
  Dockerfile                  the worker, api and seed images, built from the repo root
  dynamicconfig/dev.yaml      retention jitter, visibility flush interval
  bootstrap.sh                namespace + search attributes (idempotent)
  reap.sh                     force-delete closed executions
  reset.sh                    delete every customer workflow (make reset)
  inspect/                    canned Postgres/ES queries (docs/DATASTORES.md)
Makefile
```

## Docs

- [docs/DATASTORES.md](docs/DATASTORES.md) — how Temporal uses Postgres (persistence) and
  Elasticsearch (visibility), including an end-to-end write trace
