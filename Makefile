# Rewards POC. Every stack setting lives in deploy/docker-compose.yml,
# written out literally -- there is no env file.

COMPOSE := docker compose -f deploy/docker-compose.yml

# Host-side ports, repeated from docker-compose.yml for the banner and curls.
API_PORT = 8081
WEB_PORT = 5173
TEMPORAL_UI_PORT = 8080

.PHONY: help up down destroy bootstrap logs ps tools \
        worker worker-stop api test workflowcheck enroll status add deactivate \
        audit web web-check seed reset

# The temporal CLI ships in the server image; exec-ing into it beats a
# separate container by several seconds per invocation.
TCTL = $(COMPOSE) exec -T temporal

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

up: ## Start the whole stack: bootstrap, worker, API, UI, demo customers
	$(COMPOSE) up -d --wait postgres elasticsearch temporal temporal-ui
	@$(MAKE) --no-print-directory bootstrap
# After bootstrap, so neither talks to a namespace that doesn't exist yet.
	$(COMPOSE) up -d --build --wait worker api
	@$(MAKE) --no-print-directory seed
# The UI starts last and is not waited on: a first start installs npm
# packages for minutes, and nothing else needs it. `make logs SVC=web` shows
# progress.
	$(COMPOSE) up -d web
	@echo
	@echo "React UI:     http://localhost:$(WEB_PORT) (starting -- make logs SVC=web)"
	@echo "Temporal UI:  http://localhost:$(TEMPORAL_UI_PORT)"
	@echo "HTTP API:     http://localhost:$(API_PORT)/api/customers"

down: ## Stop the stack, keep data
	$(COMPOSE) down

destroy: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

bootstrap: ## Create namespace + search attributes (idempotent)
	@$(TCTL) bash /bootstrap.sh

logs: ## Tail logs (make logs SVC=temporal)
	$(COMPOSE) logs -f $(SVC)

ps: ## Show stack status
	$(COMPOSE) ps

tools: ## Shell in the temporal container (temporal CLI on PATH)
	$(COMPOSE) exec temporal bash

# For when workflow code has changed under live executions and the dev answer
# is to start over.
reset: ## Delete EVERY customer workflow, running included (dev only)
	@$(TCTL) bash /reset.sh

test: ## Run the Go unit tests
	go test ./...

# The Go SDK has no workflow sandbox: `time.Now()` in workflow code compiles
# and passes the unit tests, then wedges a customer on replay in production.
# workflowcheck is the static analysis that catches it.
#
# GOTOOLCHAIN is derived from the repo's effective toolchain because
# workflowcheck type-checks dependencies in-process, and a binary built with
# an older Go fails on every one of them.
WORKFLOWCHECK_VERSION = v0.5.0
WORKFLOWCHECK = $(shell go env GOPATH)/bin/workflowcheck

workflowcheck: ## Static determinism check on workflow code
	@GOTOOLCHAIN=$$(go env GOVERSION) \
	  go install go.temporal.io/sdk/contrib/tools/workflowcheck@$(WORKFLOWCHECK_VERSION)
	$(WORKFLOWCHECK) ./...

# The worker and api are Compose services built from deploy/Dockerfile, so a
# code change is a rebuild: --build recompiles, `up -d` recreates.
worker: ## Rebuild and restart the worker with the current code
	$(COMPOSE) up -d --build worker

worker-stop: ## Stop the worker (leaves the rest of the stack up)
	$(COMPOSE) stop worker

api: ## Rebuild and restart the HTTP API with the current code
	$(COMPOSE) up -d --build --wait api

# web/ is bind-mounted, so edits hot-reload without restarting anything.
web: ## Start (or restart) the Vite UI in the stack
	$(COMPOSE) up -d web

web-check: ## Typecheck and production-build the UI (the dev server doesn't)
	$(COMPOSE) exec -T web npm run build

# The CLI targets below drive the whole workflow with no API or UI.
# Addressed as temporal:7233 because the frontend binds the container's own
# interface; the namespace comes from the container's environment.
ID ?= c-001
TCLI = $(COMPOSE) exec -T \
         -e TEMPORAL_ADDRESS=temporal:7233 \
         temporal temporal

enroll: ## Enroll a customer (make enroll ID=c-001 NAME="Ada")
	@$(TCLI) workflow start \
	  --task-queue rewards \
	  --type CustomerRewardsWorkflow \
	  --workflow-id customer-$(ID) \
	  --id-conflict-policy Fail \
	  --input '{"customerId":"$(ID)","name":"$(or $(NAME),Ada Lovelace)"}'

status: ## Query a customer's status (make status ID=c-001)
	@$(TCLI) workflow query --workflow-id customer-$(ID) --type getStatus

add: ## Add points (make add ID=c-001 AMOUNT=100 REASON=purchase)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name addPoints \
	  --input '{"amount":$(or $(AMOUNT),100),"reason":"$(or $(REASON),purchase)"}'

deactivate: ## Leave the program for good; one-way, completes the workflow (make deactivate ID=c-001)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name deactivate

# Through the API rather than the temporal CLI, because the audit log is not
# something the server can be asked for -- it is reconstructed by crawling
# Event History. Compare with the raw events:
#   $(COMPOSE) exec temporal temporal workflow show --workflow-id customer-c-001
audit: ## Show the reconstructed audit timeline (make audit ID=c-001)
	@curl -sf localhost:$(API_PORT)/api/customers/$(ID)/audit \
	  || { echo "no API on :$(API_PORT) -- check 'make ps'" >&2; exit 1; }

# Runs in the stack off the same image as the worker and api, so `make up`
# needs Docker and nothing else.
seed: ## Seed demo customers (idempotent; `make reset` first for a clean slate)
	$(COMPOSE) run --rm -T --build seed
