# Architecture diagrams

Companion to the [README](../README.md): the same design, drawn. Each section
names the code it depicts.

## The stack

Everything runs in [docker-compose.yml](../deploy/docker-compose.yml). The
central claim of the POC is visible in the shape: the API's only backend is
Temporal — Postgres and Elasticsearch sit *behind* Temporal (persistence and
visibility respectively), not behind the application.

```mermaid
flowchart LR
    browser["Browser"]

    subgraph compose["docker compose (name: rewards-poc)"]
        web["web\nVite dev server :5173\nproxies /api"]
        api["api :8081\nTemporal client only —\nno DB, no cache, no ORM"]
        worker["worker\npolls task queue 'rewards',\nruns CustomerRewardsWorkflow"]
        temporal["temporal :7233"]
        temporalui["temporal-ui :8080"]
        postgres[("Postgres\npersistence:\nEvent History")]
        es[("Elasticsearch\nvisibility:\nsearch attributes")]
        seed["seed (one-shot,\ndemo data via the API)"]
    end

    browser --> web
    browser --> temporalui
    web -->|"/api"| api
    api -->|gRPC| temporal
    temporalui --> temporal
    worker <-->|"poll / complete tasks"| temporal
    temporal --> postgres
    temporal --> es
    seed --> api
```

The API's responses carry deep links into the Temporal UI, which is why the
browser talks to both.

## Workflow lifecycle

[`CustomerRewardsWorkflow`](../internal/rewards/workflows/workflow.go) — one
long-lived Entity Workflow per customer, in which each *run* is one link of a
continue-as-new chain. Two details worth reading off the picture: enrollment
validation failing *fails the run* (which is what lets
`ALLOW_DUPLICATE_FAILED_ONLY` allow a retry), and the deactivated flag is
checked *after* the handler drain, so a deactivate landing while a due roll
waits for handlers still completes the workflow instead of rolling into a run
nothing can ever wake.

```mermaid
stateDiagram-v2
    [*] --> Validating : start (run 1) or continue-as-new (run N)
    Validating --> Failed : payload refused — non-retryable error
    Failed --> [*] : run Failed (the ID may be retried)

    Validating --> Accepting : handlers registered, search attributes upserted
    Accepting : each accepted addPoints increments earnsThisRun
    Accepting --> Draining : earnsThisRun reaches 3, or deactivate committed
    Draining : awaits AllHandlersFinished

    Draining --> Completed : deactivated (checked after the drain)
    Completed --> [*] : run Completed — permanent, balance frozen

    Draining --> Rolling : not deactivated
    Rolling --> [*] : continue-as-new — runNumber+1, state carried forward
```

Three earns per run is [`EarnsPerRun`](../internal/rewards/state.go), a demo
number; production would await `GetContinueAsNewSuggested()`.

## The addPoints Update

The round-trip behind `POST /api/customers/{id}/points`, showing the
validator/handler split: both rejections look identical to the caller (a 422
with code `rejected`), but only the handler's leaves a trace in Event History.
Also visible: why the customer *list* lags a write by ~200–300 ms (the search
attribute upsert is indexed asynchronously) while the detail page, which is a
Query, never does.

```mermaid
sequenceDiagram
    participant C as Client
    participant A as API
    participant T as Temporal server
    participant W as Worker
    participant ES as Elasticsearch

    C->>A: POST /api/customers/{id}/points {amount, reason}
    A->>T: UpdateWorkflow(addPoints)
    T->>W: deliver Update
    W->>W: validator — facts about the request:<br/>amount in (0, 1000], reason set

    alt validator rejects
        W-->>T: rejected — nothing written to Event History
        T-->>A: error
        A-->>C: 422 rejected (invisible in the audit log, by design)
    else validator passes
        Note over T: UpdateAccepted written to Event History
        W->>W: handler — facts about the customer:<br/>cap of 5000 not breached

        alt handler rejects
            W-->>T: non-retryable ApplicationError (recorded)
            T-->>A: error
            A-->>C: 422 rejected (audit log shows points_rejected)
        else success
            W->>W: points += amount
            W->>T: upsert search attributes
            T--)ES: index (async — list reflects it ~200–300 ms later)
            W-->>T: UpdateCompleted {balance, level}
            T-->>A: result
            A-->>C: 200 {balance, level}
        end
    end
```

## The run chain and the audit crawl

Nothing stores a customer's point-add history. `GET /api/customers/{id}/audit`
([internal/audit](../internal/audit/)) fetches the current run's history and
walks *backwards* through the chain via each run's previous-run pointer,
reconstructing the timeline from events Temporal recorded anyway. Closed runs
are reaped after retention (1 h here, Temporal's minimum), so the walk can
stop short of the enrollment run — reported as truncation, quantified against
the `lifetimeEarnEvents` counter carried in workflow state, which survives
reaping because state rolls forward even though history does not.

```mermaid
flowchart LR
    subgraph chain["workflow ada-lovelace (one continue-as-new chain)"]
        r1["run 1 — enrollment\nclosed, history reaped ✕"]
        r2["run 2\nclosed, history retained"]
        r3["run 3\nopen"]
        r1 -->|continue-as-new| r2 -->|continue-as-new| r3
    end

    crawl["audit crawl\nGET /api/customers/ada-lovelace/audit"]
    crawl -.->|"1: fetch history of current run"| r3
    crawl -.->|"2: follow previousRunId"| r2
    crawl -.->|"3: history gone → stop,\ntruncated: true"| r1

    timeline["Timeline: enrolled · points_added ·\npoints_rejected · run_rolled · deactivated\n+ 'showing 3 of 21'"]
    crawl --> timeline
```
