# rewards-poc

A POC of the **Entity Workflow** pattern: **Temporal as the system of record**
for a customer rewards program. Written for developers who already know
Temporal basics and want to see what using it as the *data store* looks like.

There is no application database. A customer's points, tier, enrollment date,
and history of point-earning events live entirely in one long-lived workflow
and its Event History:

- points arrive as Updates ([`addPoints`](internal/rewards/contract.go), with
  a validator),
- current state is a Query ([`getStatus`](internal/rewards/contract.go)),
- the customer list is a visibility query over custom
  [search attributes](internal/rewards/searchattr.go),
- the audit log is reconstructed by [crawling Event History](internal/audit/),
- the workflow continues-as-new to keep history bounded (after every 3
  successful point-adds here — unrealistically often, so the rollover is easy
  to watch).

The API holds a Temporal client and nothing else — no database, no cache, no
ORM.

## Quick start

The only prerequisite is **Docker** with Compose v2 — every process runs in
[the stack](deploy/docker-compose.yml). (Go 1.25.4+ is needed only for
`go test`.) Tests are intentionally light: nothing that does not teach
something about the pattern.

```sh
make up   # the whole stack; takes a couple of minutes the first time
```

| Service | URL |
|---|---|
| Web App | <http://localhost:5173> |
| HTTP API | <http://localhost:8081/api/customers> |
| Temporal UI | <http://localhost:8080> |

`make up` again rebuilds the worker and API after a Go change — until then the
stack keeps running the old code. `make logs SVC=worker` (or `api`, or `web`)
tails a service, `make destroy` tears the stack down and deletes its volumes,
and `make help` lists everything.

Start with the [Web App](http://localhost:5173/) in the browser to exercise the
flow. The UI is disposable scaffolding — the interesting parts are under
[`internal/httpapi`](internal/httpapi/) and
[`internal/rewards`](internal/rewards/).

## Diagrams

See [docs/architecture.md](docs/architecture.md).

## The HTTP API

Six routes — all in [`server.go`](internal/httpapi/server.go) — and each
one is a thin wrapper over a single Temporal primitive:

| Route | Temporal call behind it |
|---|---|
| `POST /api/customers` | start the customer's workflow |
| `GET /api/customers` | `ListWorkflow` + `CountWorkflow` over search attributes |
| `GET /api/customers/{id}` | the `getStatus` Query (plus a Describe for run status) |
| `POST /api/customers/{id}/points` | the `addPoints` Update |
| `DELETE /api/customers/{id}` | the `deactivate` Update |
| `GET /api/customers/{id}/audit` | an Event History crawl across the run chain |

```sh
# Enroll answers with the customer ID (here, ada-lovelace), which the
# per-customer routes below take in the path. A repeat signup is a 409.
curl -XPOST localhost:8081/api/customers -d '{"name":"Ada Lovelace"}'

curl localhost:8081/api/customers/ada-lovelace
curl -XPOST localhost:8081/api/customers/ada-lovelace/points -d '{"amount":500,"reason":"purchase"}'
curl localhost:8081/api/customers/ada-lovelace/audit
curl -XDELETE localhost:8081/api/customers/ada-lovelace
```

The points body also takes an optional `requestId` — the caller's idempotency
key. It becomes the Temporal Update ID, which the server dedups within a run,
and it rides in the Update argument so the workflow can dedupe *across* runs
too: each successful add records its ID in a bounded ring in
[`CustomerState`](internal/rewards/state.go), carried forward by
continue-as-new, and the next run's validator rejects a replay before it
writes any history. Either way a retry answers 200 with the current balance
instead of double-applying (see "Idempotency across the roll" below).

The list is filterable — no lookup table. The server builds the visibility
query from structured params ([`filter.go`](internal/httpapi/filter.go)) and
echoes it in the response, **pasteable into
the Temporal UI unchanged** — plus a `queryUrl`
that opens the Temporal UI already filtered by it:

```sh
curl -sG localhost:8081/api/customers --data-urlencode "tier=gold"
curl -sG localhost:8081/api/customers --data-urlencode "status=deactivated"
curl -sG localhost:8081/api/customers --data-urlencode "name=ada"   # word-prefix match
```

Failures are `{"error":{"code":"...","message":"..."}}` with a stable code
([`errors.go`](internal/httpapi/errors.go)) — notably `worker_unavailable` (503) when nothing is polling the task queue,
`rejected` (422) when the workflow refused a request, and `deactivated` (409)
when re-enrolling a retired customer ID.

## Things worth seeing

**Continue-as-new.** Every 3 successful point-adds the workflow ends its run
and starts a fresh one carrying state forward:

```sh
curl -XPOST localhost:8081/api/customers -d '{"name":"Rolly Poly"}'
for i in 1 2 3 4 5 6 7; do
  curl -XPOST localhost:8081/api/customers/rolly-poly/points \
    -d "{\"amount\":100,\"reason\":\"add $i\"}"
done
curl localhost:8081/api/customers/rolly-poly   # runNumber 3, points 700
```

Three is a demo number chosen to be watchable
([`EarnsPerRun`](internal/rewards/state.go)); production should ask
`workflow.GetInfo(ctx).GetContinueAsNewSuggested()`.

**Idempotency across the roll.** Temporal dedups Update IDs within a single
run, but an Entity Workflow rolls over — a retry that lands after the
continue-as-new reaches a fresh run where the server has never seen the ID.
The workflow closes that hole itself: successful adds record their
`requestId` in a bounded ring carried in state, and the next run's
*validator* rejects a replay — so the retry writes nothing to the new run's
history. Watch it straddle a roll:

```sh
curl -XPOST localhost:8081/api/customers -d '{"name":"Dee Dupe"}'
for i in 1 2; do
  curl -XPOST localhost:8081/api/customers/dee-dupe/points \
    -d "{\"amount\":100,\"reason\":\"add $i\",\"requestId\":\"key-$i\"}"
done
# The 3rd add rolls the run; the "retry" of key-3 arrives at run 2.
curl -XPOST localhost:8081/api/customers/dee-dupe/points \
  -d '{"amount":100,"reason":"add 3","requestId":"key-3"}'
curl -XPOST localhost:8081/api/customers/dee-dupe/points \
  -d '{"amount":100,"reason":"add 3","requestId":"key-3"}'
curl localhost:8081/api/customers/dee-dupe   # points 300, not 400
```

Both sends answer 200 with `balance: 300`, and run 2's history shows one
enrollment and nothing else. Only the IDs ride the roll, so the deduped
retry is answered with the *current* balance rather than a replay of the
original result; a system needing exact replay would carry (id, result)
pairs instead.

**The audit log is the Event History.** Nothing stores a customer's point-add
history; `GET /api/customers/<id>/audit` walks back through the run chain
([`audit`](internal/audit/)) and reads the events Temporal
recorded anyway. Closed runs are deleted after
retention (1 hour here, Temporal's minimum), so the response reports
truncation — "showing 3 of 21" — rather than quietly showing less.

**The [validator/handler split](internal/rewards/workflows/workflow.go).**
Both of these fail identically from the caller's side, but only one leaves a
trace:

```sh
# validator: writes no history at all
curl -XPOST localhost:8081/api/customers/ada-lovelace/points -d '{"amount":-50,"reason":"oops"}'
# handler: recorded, shows as points_rejected (seeded customer `max-capacity` is at 4960)
curl -XPOST localhost:8081/api/customers/max-capacity/points -d '{"amount":100,"reason":"over cap"}'
```

A validator rejection writes no events — a client stuck retrying `amount: -1`
cannot grow history — while a rejection that depends on the customer's
accumulated state is permanently recorded. Facts about the *request* belong in
the validator, facts about the *customer* in the handler.

**Deactivation completes the workflow.** Leaving the program is one-way: the
`deactivate` Update sets the flag and the workflow completes normally with the
balance frozen in its final state. The detail page, list, and audit log keep
answering for a departed customer (Query and Describe work on closed runs
until retention reaps them), and the enroll endpoint refuses to reuse the ID —
`ALLOW_DUPLICATE_FAILED_ONLY` retires a completed execution's ID while still
letting a *failed* enrollment be retried. The retirement lasts as long as the
completed run's history does: once retention reaps it, the ID is enrollable
again — an artifact of the 1-hour demo retention.

**No Activities, deliberately.** Nothing in the rewards program touches the
outside world — the workflow is a pure state machine and Temporal is its
store, which is the argument of the POC. A real system would notify customers
on promotion via an Activity.

## Behaviour to expect

- **Points only go up.** No spending or expiry, so tiers never demote and
  `Points` is also the lifetime total.
- **Visibility is asynchronous.** A new or updated workflow appears in the
  list after ~200–300 ms, never instantly. Read-after-write goes through
  Query or Describe instead.

## Layout

```
cmd/                worker, api, and seed (demo data) processes
internal/rewards/   the domain: state, tiers, the Update/Query contract
  workflows/        CustomerRewardsWorkflow
internal/audit/     Event History crawl that rebuilds the audit timeline
internal/httpapi/   HTTP handlers over Temporal (and the audit crawl)
web/                the web app (React)
docs/               architecture diagrams
deploy/             docker-compose.yml (every setting, literally), Dockerfile
Makefile
```
