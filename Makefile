# Rewards POC
#
# Every target runs against one stack, selected by ENV. Default is .env when
# present, otherwise .env.example (so a fresh checkout can `make up` with no
# copy step). For a second stack side by side, copy .env.example to .env.beta
# with a different COMPOSE_PROJECT_NAME and ports, then: make up ENV=.env.beta

ENV ?= $(shell test -f .env && echo .env || echo .env.example)
COMPOSE := docker compose --env-file $(ENV) -f deploy/docker-compose.yml

# Pull TEMPORAL_* out of the env file so host-side targets can use them.
NAMESPACE = $(shell grep -E '^TEMPORAL_NAMESPACE=' $(ENV) | cut -d= -f2)
RETENTION = $(shell grep -E '^TEMPORAL_RETENTION=' $(ENV) | cut -d= -f2)
REFRESH   = $(shell grep -E '^ES_REFRESH_INTERVAL=' $(ENV) | cut -d= -f2)
UI_PORT   = $(shell grep -E '^TEMPORAL_UI_PORT=' $(ENV) | cut -d= -f2)
API_PORT  = $(shell grep -E '^API_PORT=' $(ENV) | cut -d= -f2)
WEB_PORT  = $(shell grep -E '^WEB_PORT=' $(ENV) | cut -d= -f2)

.PHONY: help up down destroy bootstrap logs ps psql es tools verify-config reap \
        worker worker-logs worker-stop api api-logs api-stop test workflowcheck enroll status add deactivate reactivate \
        inspect inspect-pg inspect-es write-trace audit web web-logs web-check web-stop seed reset

# Most host-side targets just need the temporal CLI against the running server.
# The CLI ships in the server image, and exec-ing beats `compose run` on a
# separate container by several seconds per invocation.
TCTL = $(COMPOSE) exec -T temporal

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

$(ENV):
	@echo "No $(ENV) found. Defaults live in .env.example; for a local override run: cp .env.example $(ENV)" >&2; exit 1

up: $(ENV) ## Start the whole stack: bootstrap, worker, API, UI, demo customers
	$(COMPOSE) up -d --wait postgres elasticsearch temporal temporal-ui
	@$(MAKE) --no-print-directory bootstrap ENV=$(ENV)
# Started after bootstrap, so neither one talks to a namespace that doesn't
# exist yet; --build so a fresh checkout runs code built from its own tree.
# --wait holds until the API answers /healthz, which is what seeding needs.
	$(COMPOSE) up -d --build --wait worker api
# Idempotent, so `make up` on a stack that already has its customers reports
# them and changes nothing. A mismatch fails the target on purpose -- it means
# the running dataset is not the one in cmd/seed, and only `make reset` fixes
# that. The stack is up either way.
	@$(MAKE) --no-print-directory seed ENV=$(ENV)
# Last, and deliberately not waited on. A first start installs a few hundred
# npm packages over a bind mount, which is minutes on Docker Desktop, and
# nothing else here needs the UI -- blocking on it would hold up seeding and
# make an npm problem look like a broken stack. `make web-logs` shows progress;
# `make ps` shows when it is serving.
	$(COMPOSE) up -d web
	@echo
	@echo "React UI:     http://localhost:$(WEB_PORT) (starting -- make web-logs)"
	@echo "Temporal UI:  http://localhost:$(UI_PORT)"
	@echo "HTTP API:     http://localhost:$(API_PORT)/api/customers"
	@echo "Namespace:    $(NAMESPACE) (retention $(RETENTION))"

down: $(ENV) ## Stop the stack, keep data
	$(COMPOSE) down

destroy: $(ENV) ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

bootstrap: $(ENV) ## Create namespace + search attributes (idempotent)
	@$(TCTL) env TEMPORAL_NAMESPACE=$(NAMESPACE) TEMPORAL_RETENTION=$(RETENTION) \
	  ES_REFRESH_INTERVAL=$(REFRESH) bash /bootstrap.sh

logs: $(ENV) ## Tail logs (make logs SVC=temporal)
	$(COMPOSE) logs -f $(SVC)

ps: $(ENV) ## Show stack status
	$(COMPOSE) ps

tools: $(ENV) ## Shell in the temporal container (temporal CLI on PATH)
	$(COMPOSE) exec temporal bash

# Interactive shell, or a canned query: make psql Q=history-blob ID=inspect
psql: $(ENV) ## psql into Temporal persistence (Q=… for canned inspect queries)
ifeq ($(Q),)
	$(COMPOSE) exec postgres psql -U temporal -d temporal
else
	@$(MAKE) --no-print-directory inspect-pg ENV=$(ENV) Q=$(Q) ID=$(ID)
endif

# Index summary, or a canned query: make es Q=mapping
es: $(ENV) ## Elasticsearch summary (Q=… for canned inspect queries)
ifeq ($(Q),)
	@$(TCTL) curl -s "http://elasticsearch:9200/_cat/indices/temporal_visibility*?v&h=index,docs.count,store.size,docs.deleted"
else
	@$(MAKE) --no-print-directory inspect-es ENV=$(ENV) Q=$(Q) ID=$(ID)
endif

verify-config: $(ENV) ## Check the platform behaviour the plan depends on
	@$(TCTL) bash /inspect/verify-config.sh

reap: $(ENV) ## Delete closed executions now (make reap WF=customer-x)
	@$(TCTL) env TEMPORAL_NAMESPACE=$(NAMESPACE) WF="$(WF)" bash /reap.sh

# Distinct from reap, which spares running executions on purpose. This one takes
# everything, for when workflow code has changed under live executions and the
# dev answer is to start over.
reset: $(ENV) ## Delete EVERY customer workflow, running included (dev only)
	@$(TCTL) env TEMPORAL_NAMESPACE=$(NAMESPACE) bash /reset.sh

# --- Datastore inspection -----------------------------------------------------
# Canned queries live in deploy/inspect/. Docs: docs/DATASTORES.md.
# ID defaults to the same customer the CLI targets (inspect → customer-inspect).

inspect: ## List canned Postgres/ES inspect queries
	@echo "Postgres (make inspect-pg Q=… ID=$(ID)):"
	@echo "  history-blob      opaque history_node blobs"
	@echo "  current-run       continue-as-new indirection"
	@echo "  visibility-tasks  async queue feeding ES"
	@echo "  after-reap        rows before/after make reap"
	@echo
	@echo "Elasticsearch (make inspect-es Q=… ID=$(ID)):"
	@echo "  mapping           index mapping with custom SAs"
	@echo "  customer          docs for one WorkflowId"
	@echo "  gold-running      list-page filter + ES-side sort"
	@echo "  indices           index size / searchable count"
	@echo "  closed            one customer's docs, before/after reap"
	@echo
	@echo "End-to-end: make write-trace ID=$(ID) AMOUNT=10"
	@echo "Docs:       docs/DATASTORES.md"

inspect-pg: $(ENV) ## Run a Postgres inspect query (make inspect-pg Q=history-blob ID=inspect)
	@case "$(Q)" in \
	  history-blob) f=pg-01-history-blob.sql ;; \
	  current-run) f=pg-02-current-run.sql ;; \
	  visibility-tasks) f=pg-03-visibility-tasks.sql ;; \
	  after-reap) f=pg-04-after-reap.sql ;; \
	  *) echo "Unknown Q='$(Q)'. Run: make inspect" >&2; exit 1 ;; \
	 esac; \
	 echo "# deploy/inspect/$$f  (wf=customer-$(ID))"; \
	 $(COMPOSE) exec -T postgres \
	   psql -U temporal -d temporal -v ON_ERROR_STOP=1 -v wf=customer-$(ID) \
	   < deploy/inspect/$$f

inspect-es: $(ENV) ## Run an ES inspect query (make inspect-es Q=mapping ID=inspect)
	@case "$(Q)" in \
	  mapping) f=es-01-mapping.sh ;; \
	  customer) f=es-02-customer.sh ;; \
	  gold-running) f=es-03-gold-running.sh ;; \
	  indices) f=es-04-indices.sh ;; \
	  closed) f=es-05-closed.sh ;; \
	  *) echo "Unknown Q='$(Q)'. Run: make inspect" >&2; exit 1 ;; \
	 esac; \
	 echo "# deploy/inspect/$$f  (WF=customer-$(ID))"; \
	 $(TCTL) env WF=customer-$(ID) bash /inspect/$$f

write-trace: $(ENV) ## Trace one addPoints through Postgres + ES (make write-trace ID=inspect)
	@NAMESPACE=$(NAMESPACE) ENV=$(ENV) ID=$(ID) AMOUNT=$(or $(AMOUNT),10) \
	  bash deploy/inspect/write-trace.sh

# --- Workflow (Phase 1) -----------------------------------------------------

test: ## Run the Go unit tests
	go test ./...

# The Go SDK has no workflow sandbox: `time.Now()` in workflow code compiles,
# passes vet, and passes the unit tests, then wedges a customer on replay weeks
# later. workflowcheck is the static analysis that catches it -- it walks the
# call graph from every function taking a workflow.Context and flags anything
# reaching a non-deterministic call.
#
# Pinned rather than @latest so a tool upgrade is a commit and not a surprise.
#
# GOTOOLCHAIN is derived from the repo's own effective toolchain, and is
# load-bearing. workflowcheck type-checks our dependencies in-process, so a
# binary built with an older Go than they require fails on every one of them
# ("package requires newer Go version") rather than on anything real -- and the
# system Go here is older than the go.mod toolchain the repo actually builds
# with. Deriving it means this keeps working when that directive moves.
WORKFLOWCHECK_VERSION = v0.5.0
WORKFLOWCHECK = $(shell go env GOPATH)/bin/workflowcheck

workflowcheck: ## Static determinism check on workflow code
	@GOTOOLCHAIN=$$(go env GOVERSION) \
	  go install go.temporal.io/sdk/contrib/tools/workflowcheck@$(WORKFLOWCHECK_VERSION)
	$(WORKFLOWCHECK) ./...

# The worker and the api are Compose services (deploy/Dockerfile), started by
# `make up` and isolated per stack by COMPOSE_PROJECT_NAME like everything else.
# A code change is therefore a rebuild rather than a Ctrl-C, and these targets
# are both halves of it: --build recompiles, `up -d` recreates the container.
# What is running is never older than the source tree it was last built from.
worker: $(ENV) ## Rebuild and restart the worker with the current code
	$(COMPOSE) up -d --build worker

worker-logs: $(ENV) ## Tail the worker's logs (Ctrl-C to stop tailing)
	$(COMPOSE) logs -f worker

# `compose stop` is an explicit stop, so restart: unless-stopped leaves it down
# until `make worker` brings it back. Handy for watching the API's 503
# worker_unavailable path.
worker-stop: $(ENV) ## Stop the worker (leaves the rest of the stack up)
	$(COMPOSE) stop worker

api: $(ENV) ## Rebuild and restart the HTTP API with the current code
	$(COMPOSE) up -d --build --wait api

api-logs: $(ENV) ## Tail the API's logs (Ctrl-C to stop tailing)
	$(COMPOSE) logs -f api

api-stop: $(ENV) ## Stop the API (leaves the rest of the stack up)
	$(COMPOSE) stop api

# The Vite dev server is a Compose service too, with web/ bind-mounted, so an
# edit still hot-reloads without restarting anything. It installs dependencies on
# the way up, so the first start is minutes and later ones are seconds; nothing
# waits on it, so that cost lands on the UI alone.
#
# Its serve port, proxy target and Temporal UI URL come from $(ENV) via compose,
# so `make web ENV=.env.beta` serves on beta's WEB_PORT and points at beta's API
# and Temporal UI. A shell variable outranks web/.env* in Vite's loadEnv, so this
# wins over a local override file.
web: $(ENV) ## Start (or restart) the Vite UI in the stack
	$(COMPOSE) up -d web

web-logs: $(ENV) ## Tail the Vite dev server's logs (Ctrl-C to stop tailing)
	$(COMPOSE) logs -f web

# The typecheck the container used to run on every start. Vite's dev server does
# not typecheck, so this is the only thing that does -- run it before believing
# the UI compiles.
web-check: $(ENV) ## Typecheck and production-build the UI (tsc -b && vite build)
	$(COMPOSE) exec -T web npm run build

web-stop: $(ENV) ## Stop the UI (leaves the rest of the stack up)
	$(COMPOSE) stop web

# The CLI targets below are the Phase 1 acceptance path: the whole workflow is
# drivable without an API or UI. ID=<customer id> selects the customer.
#
# Addressed as temporal:7233, not localhost: the frontend binds the container's
# own interface, so loopback is refused even from inside its own container --
# the same quirk that made the Phase 0 healthcheck fail.
ID ?= c-001
TCLI = $(COMPOSE) exec -T \
         -e TEMPORAL_ADDRESS=temporal:7233 \
         -e TEMPORAL_NAMESPACE=$(NAMESPACE) \
         temporal temporal

enroll: $(ENV) ## Enroll a customer (make enroll ID=c-001 NAME="Ada")
	@$(TCLI) workflow start \
	  --task-queue rewards \
	  --type CustomerRewardsWorkflow \
	  --workflow-id customer-$(ID) \
	  --id-conflict-policy Fail \
	  --input '{"customerId":"$(ID)","name":"$(or $(NAME),Ada Lovelace)"}'

status: $(ENV) ## Query a customer's status (make status ID=c-001)
	@$(TCLI) workflow query --workflow-id customer-$(ID) --type getStatus

add: $(ENV) ## Add points (make add ID=c-001 AMOUNT=100 REASON=purchase)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name addPoints \
	  --input '{"amount":$(or $(AMOUNT),100),"reason":"$(or $(REASON),purchase)"}'

deactivate: $(ENV) ## Soft-leave the program (make deactivate ID=c-001)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name deactivate

reactivate: $(ENV) ## Re-enroll and restore points (make reactivate ID=c-001)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name reactivate

# The one target that goes through the API rather than the temporal CLI, because
# the audit log is not a thing the server can be asked for -- it is reconstructed
# by crawling Event History. Compare with the raw events behind it:
#
#   make audit ID=c-001
#   $(COMPOSE) exec temporal temporal workflow show --workflow-id customer-c-001
audit: $(ENV) ## Show the reconstructed audit timeline (make audit ID=c-001)
	@curl -sf localhost:$(API_PORT)/api/customers/$(ID)/audit \
	  || { echo "no API on :$(API_PORT) -- check 'make ps'" >&2; exit 1; }

# Fills a running stack with a demo dataset, driving the HTTP API rather than
# the Temporal client so seeding exercises the path a user takes -- rollover
# retries and error mapping included.
#
# Read-then-create: it only enrolls customers who are missing, and reports any
# existing one whose balance disagrees with the dataset. Deactivation is soft,
# so nothing here can reset a customer -- `make reset` is the clean slate.
#
# `make up` runs this, so the target is for re-running it after a `make reset`.
#
# It runs in the stack rather than on the host, off the same image as the worker
# and the api, so `make up` needs Docker and nothing else -- no Go toolchain for
# a step that is only meant to fill a demo stack. `run --rm` because it exits;
# --build so it is never older than cmd/seed; -T because there is no terminal to
# attach to when make calls it.
seed: $(ENV) ## Seed demo customers (idempotent; `make reset` first for a clean slate)
	$(COMPOSE) run --rm -T --build seed
