# rewards-poc

A POC of the [**Entity Workflow** pattern](https://temporal.io/blog/very-long-running-workflows): **Temporal as the system of record**
for a customer rewards program. Written for developers who already know
Temporal basics and want to see what using it as the *data store* looks like.

There is no application database. A customer's points, tier, enrollment date,
and history of point-earning events live entirely in one long-lived workflow
(whose ID is the customer's dashed name, e.g. `ada-lovelace`) and its Event
History:

- points arrive as [Updates](https://docs.temporal.io/develop/go/message-passing) ([`addPoints`](internal/rewards/contract.go), with
  a validator),
- current state is a Query ([`getStatus`](internal/rewards/contract.go)),
- the customer list is a visibility query over custom
  [search attributes](internal/rewards/searchattr.go),
- the audit log is reconstructed by crawling Event History,
- the workflow [continues-as-new](https://docs.temporal.io/workflow-execution/continue-as-new) to keep history bounded (after every 3
  updates here — unrealistically often, so the rollover is easy to watch).

The API holds a Temporal client and nothing else — no database, no cache, no
ORM.

## Quick start

The only prerequisite is **Docker** with Compose v2 — every process runs in
[the stack](deploy/docker-compose.yml). (Go 1.25.4+ is needed only for
`go test`.) Tests are intentionally light: nothing that does not teach
something about the pattern.

```sh
make up   # the whole stack; a couple of minutes the first time
```

| Service | URL |
|---|---|
| [React UI](web/) | <http://localhost:5173> |
| HTTP API | <http://localhost:8081/api/customers> |
| Temporal UI | <http://localhost:8080> |

`make up` again rebuilds the worker and API after a Go change — until then the
stack keeps running the old code. `make logs SVC=worker` (or `api`, or `web`)
tails a service, `make destroy` tears the stack down and deletes its volumes,
and `make help` lists everything.

## The HTTP API

Seven routes — all in [`server.go`](internal/httpapi/server.go) — and each
one is a thin wrapper over a single Temporal primitive, which is the point:

| Route | Temporal call behind it |
|---|---|
| `POST /api/customers` | start the customer's workflow |
| `GET /api/customers` | `ListWorkflow` + `CountWorkflow` over search attributes |
| `GET /api/customers/{id}` | the `getStatus` Query (plus a Describe for run status) |
| `POST /api/customers/{id}/points` | the `addPoints` Update |
| `DELETE /api/customers/{id}` | the `deactivate` Update |
| `GET /api/customers/{id}/audit` | an Event History crawl across the run chain |
| `GET /healthz` | nothing — liveness only |

```sh
# No customerId: the server derives one from the name (here, ada-lovelace).
# The same name derives the same ID, so a second signup is a 409.
curl -XPOST localhost:8081/api/customers -d '{"name":"Ada Lovelace"}'

curl localhost:8081/api/customers/ada-lovelace
curl -XPOST localhost:8081/api/customers/ada-lovelace/points -d '{"amount":500,"reason":"purchase"}'
curl localhost:8081/api/customers/ada-lovelace/audit
curl -XDELETE localhost:8081/api/customers/ada-lovelace
```

The list is filterable — no lookup table. The server builds the visibility
query from structured params ([`filter.go`](internal/httpapi/filter.go)) and
echoes it in the response, **pasteable into
the Temporal UI unchanged** — plus a `queryUrl`
([`links.go`](internal/httpapi/links.go)) that opens the Temporal UI already
filtered by it:

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
curl -XPOST localhost:8081/api/customers/ada/points -d '{"amount":-50,"reason":"oops"}'
# handler: recorded, shows as points_rejected (seeded customer `capped` is at 4960)
curl -XPOST localhost:8081/api/customers/capped/points -d '{"amount":100,"reason":"over cap"}'
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
[`ALLOW_DUPLICATE_FAILED_ONLY`](https://docs.temporal.io/workflow-execution/workflowid-runid#workflow-id-reuse-policy)
retires a completed execution's ID while still
letting a *failed* enrollment be retried.

**The determinism check.** The Go SDK has no workflow sandbox: `time.Now()` in
workflow code compiles and passes tests, then wedges a customer on replay.
[`workflowcheck`](https://pkg.go.dev/go.temporal.io/sdk/contrib/tools/workflowcheck)
catches it statically:

```sh
go install go.temporal.io/sdk/contrib/tools/workflowcheck@v0.5.0
workflowcheck ./...
```

**No Activities, deliberately.** Nothing in the rewards program touches the
outside world — the workflow is a pure state machine and Temporal is its
store, which is the argument of the POC. A real system would notify customers
on promotion, and that is what an Activity is for.

## Behaviour to expect

- **Points only go up.** No spending or expiry, so tiers never demote and
  `Points` is also the lifetime total.
- **Visibility is asynchronous.** A new or updated workflow appears in the
  list after ~200–300 ms, never instantly. Read-after-write goes through
  Query or Describe instead.
- **You cannot sort a workflow list.** Temporal rejects `ORDER BY`, so the
  list endpoint returns at most five rows, reports how many matched, and
  pushes you to filter. Leaderboards are where a separate read model would
  start earning its keep.

## Diagrams

See [docs/architecture.md](docs/architecture.md).

## Layout

```
cmd/                worker, api, and seed (demo data) processes
internal/rewards/   the domain: state, tiers, the Update/Query contract
  workflows/        CustomerRewardsWorkflow
internal/audit/     Event History crawl that rebuilds the audit timeline
internal/httpapi/   HTTP handlers over Temporal (and the audit crawl)
web/                the React UI
docs/               architecture diagrams
deploy/             docker-compose.yml (every setting, literally), Dockerfile
Makefile
```
