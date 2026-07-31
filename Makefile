# Rewards POC -- see docs/PLAN.md
#
# Every target runs against one stack, selected by ENV (default .env). To run a
# second stack side by side, copy .env.example to .env.beta with a different
# STACK_NAME and ports, then: make up ENV=.env.beta

ENV ?= .env
COMPOSE := docker compose --env-file $(ENV) -f deploy/docker-compose.yml

# Pull TEMPORAL_* out of the env file so host-side targets can use them.
NAMESPACE = $(shell grep -E '^TEMPORAL_NAMESPACE=' $(ENV) | cut -d= -f2)
RETENTION = $(shell grep -E '^TEMPORAL_RETENTION=' $(ENV) | cut -d= -f2)
REFRESH   = $(shell grep -E '^ES_REFRESH_INTERVAL=' $(ENV) | cut -d= -f2)
UI_PORT   = $(shell grep -E '^TEMPORAL_UI_PORT=' $(ENV) | cut -d= -f2)
GRPC_PORT = $(shell grep -E '^TEMPORAL_GRPC_PORT=' $(ENV) | cut -d= -f2)
API_PORT  = $(shell grep -E '^API_PORT=' $(ENV) | cut -d= -f2)

# The mock needs no env file at all -- that is the point of it.
MOCK_PORT ?= 8082

.PHONY: help up down destroy bootstrap logs ps psql es tools verify-config reap \
        worker worker-stop workers api api-stop mockapi mockapi-stop test enroll status add deactivate \
        inspect inspect-pg inspect-es write-trace web web-build

# Most host-side targets just need the temporal CLI against the running server.
# The CLI ships in the server image, and exec-ing beats `compose run` on a
# separate container by several seconds per invocation.
TCTL = $(COMPOSE) exec -T temporal

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

$(ENV):
	@echo "No $(ENV) found. Run: cp .env.example $(ENV)" >&2; exit 1

up: $(ENV) ## Start the stack and bootstrap it
	$(COMPOSE) up -d --wait postgres elasticsearch temporal temporal-ui
	@$(MAKE) --no-print-directory bootstrap ENV=$(ENV)
	@echo
	@echo "Temporal UI:  http://localhost:$(UI_PORT)"
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

# Interactive shell, or a canned §8 query: make psql Q=history-blob ID=inspect
psql: $(ENV) ## psql into Temporal persistence (Q=… for canned inspect queries)
ifeq ($(Q),)
	$(COMPOSE) exec postgres psql -U temporal -d temporal
else
	@$(MAKE) --no-print-directory inspect-pg ENV=$(ENV) Q=$(Q) ID=$(ID)
endif

# Index summary, or a canned §8 query: make es Q=mapping
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

# --- Datastore inspection (Phase 7 / PLAN.md §8) -----------------------------
# Canned queries live in deploy/inspect/. Docs: docs/DATASTORES.md.
# ID defaults to the same customer the CLI targets (inspect → customer-inspect).

inspect: ## List canned Postgres/ES inspect queries
	@echo "Postgres (make inspect-pg Q=… ID=$(ID)):"
	@echo "  history-blob      opaque history_node blobs (§8.1.1)"
	@echo "  current-run       continue-as-new indirection (§8.1.2)"
	@echo "  visibility-tasks  async queue feeding ES (§8.1.3)"
	@echo "  after-reap        rows before/after make reap (§8.1.4)"
	@echo
	@echo "Elasticsearch (make inspect-es Q=… ID=$(ID)):"
	@echo "  mapping           index mapping with custom SAs (§8.2)"
	@echo "  customer          docs for one WorkflowId (§8.2)"
	@echo "  gold-running      list-page filter + ES-side sort (§8.2)"
	@echo "  indices           index size / searchable count (§8.2)"
	@echo "  closed            deactivated customer / post-reap (§8.2)"
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

worker: $(ENV) ## Run the workflow worker in the foreground (Ctrl-C to stop)
	TEMPORAL_HOSTPORT=localhost:$(GRPC_PORT) TEMPORAL_NAMESPACE=$(NAMESPACE) \
	  go run ./cmd/worker

# `go run` execs the compiled binary out of /root/.cache/go-build/<hash>/worker,
# not a path containing "cmd/worker", so a stale worker survives the obvious
# pkill and keeps serving old code against the same task queue. That failure is
# silent and looks like a workflow bug -- see PLAN.md 12.10.
worker-stop: ## Stop every running worker, including orphaned ones
	@pkill -f 'go-build.*/worker$$' 2>/dev/null; \
	 pkill -f 'go run \./cmd/worker' 2>/dev/null; \
	 sleep 1; \
	 left=$$(pgrep -fc 'go-build.*/worker$$' 2>/dev/null || echo 0); \
	 echo "workers still running: $$left"

workers: ## List running workers (there should be at most one)
	@ps -eo pid,etimes,args | grep -E 'go-build.*/worker$$' | grep -v grep \
	  || echo "no workers running"

api: $(ENV) ## Run the HTTP API in the foreground (Ctrl-C to stop)
	TEMPORAL_HOSTPORT=localhost:$(GRPC_PORT) TEMPORAL_NAMESPACE=$(NAMESPACE) \
	  API_PORT=$(API_PORT) go run ./cmd/api

api-stop: ## Stop every running API process, including orphaned ones
	@pkill -f 'go-build.*/api$$' 2>/dev/null; \
	 pkill -f 'go run \./cmd/api' 2>/dev/null; \
	 sleep 1; echo "stopped"

# Serves the frozen contract from fixtures. No Temporal, no Docker, no .env --
# it exists so the UI can be built before the endpoints it consumes.
mockapi: ## Run the fixture API for UI development (:8082, no stack needed)
	MOCK_PORT=$(MOCK_PORT) go run ./cmd/mockapi

mockapi-stop: ## Stop every running mockapi process
	@pkill -f 'go-build.*/mockapi$$' 2>/dev/null; \
	 pkill -f 'go run \./cmd/mockapi' 2>/dev/null; \
	 sleep 1; echo "stopped"

web: ## Run the Vite UI (make mockapi first; VITE_API_BASE overrides)
	cd web && npm run dev

web-build: ## Typecheck and build the UI
	cd web && npm run build

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

enroll: $(ENV) ## Enroll a customer (make enroll ID=c-001 NAME="Ada" EMAIL=ada@example.com)
	@$(TCLI) workflow start \
	  --task-queue rewards \
	  --type CustomerRewardsWorkflow \
	  --workflow-id customer-$(ID) \
	  --id-conflict-policy Fail \
	  --input '{"customerId":"$(ID)","name":"$(or $(NAME),Ada Lovelace)","email":"$(or $(EMAIL),$(ID)@example.com)"}'

status: $(ENV) ## Query a customer's status (make status ID=c-001)
	@$(TCLI) workflow query --workflow-id customer-$(ID) --type getStatus

add: $(ENV) ## Add points (make add ID=c-001 AMOUNT=100 REASON=purchase)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name addPoints \
	  --input '{"amount":$(or $(AMOUNT),100),"reason":"$(or $(REASON),purchase)"}'

deactivate: $(ENV) ## Leave the program -- cancel, not terminate (make deactivate ID=c-001)
	@$(TCLI) workflow cancel --workflow-id customer-$(ID)
