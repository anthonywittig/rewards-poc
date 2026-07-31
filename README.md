# rewards-poc

Demonstrating **Temporal as the system of record** for a customer rewards program.

There is no application database for rewards state. A customer's points, tier, enrollment
date, and history of point-earning events live entirely in a Temporal Workflow Execution and
its Event History. See [docs/PLAN.md](docs/PLAN.md) for the full design.

**Status: Phase 0.** Local stack only — no workflow code yet.

## Quick start

Requires Docker with Compose v2. Roughly 1.5 GB of images and ~1 GB of RAM.

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
