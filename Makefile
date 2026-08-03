# Rewards POC. Every stack setting lives in deploy/docker-compose.yml,
# written out literally -- there is no env file.

COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: help up down destroy bootstrap logs ps tools reset \
        worker worker-stop test workflowcheck \
        enroll status add deactivate reactivate list history

# Host-side targets exec into the temporal container, whose image ships the
# `temporal` CLI; exec-ing beats `compose run` by several seconds per call.
TCTL = $(COMPOSE) exec -T temporal

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

up: ## Start the whole stack: Temporal + UI + the worker
	$(COMPOSE) up -d --wait postgres elasticsearch temporal temporal-ui
	@$(MAKE) --no-print-directory bootstrap
# Started after bootstrap, so the worker never polls a namespace that doesn't
# exist yet; --build so a fresh checkout runs code built from its own tree.
	$(COMPOSE) up -d --build worker
	@echo
	@echo "Temporal UI:   http://localhost:8080"
	@echo "Enroll one:    make enroll ID=c-001 NAME=\"Ada Lovelace\""

down: ## Stop the stack, keep data
	$(COMPOSE) down

destroy: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

bootstrap: ## Create namespace + search attributes (idempotent)
	@$(TCTL) bash /bootstrap.sh

logs: ## Tail logs (make logs SVC=worker)
	$(COMPOSE) logs -f $(SVC)

ps: ## Show stack status
	$(COMPOSE) ps

tools: ## Shell in the temporal container (temporal CLI on PATH)
	$(COMPOSE) exec temporal bash

reset: ## Delete EVERY customer workflow, running included (dev only)
	@$(TCTL) bash /reset.sh

# --- Tests -------------------------------------------------------------------

test: ## Run the Go unit tests (no Docker needed)
	go test ./...

# The Go SDK has no workflow sandbox: `time.Now()` in workflow code compiles,
# passes vet and the unit tests, then wedges a customer on replay weeks later.
# workflowcheck walks the call graph from every function taking a
# workflow.Context and flags anything reaching a non-deterministic call.
#
# GOTOOLCHAIN is derived from the repo's effective toolchain because
# workflowcheck type-checks our dependencies in-process: a binary built with an
# older Go than they require fails on every one of them rather than on
# anything real.
WORKFLOWCHECK_VERSION = v0.5.0
WORKFLOWCHECK = $(shell go env GOPATH)/bin/workflowcheck

workflowcheck: ## Static determinism check on workflow code (no Docker needed)
	@GOTOOLCHAIN=$$(go env GOVERSION) \
	  go install go.temporal.io/sdk/contrib/tools/workflowcheck@$(WORKFLOWCHECK_VERSION)
	$(WORKFLOWCHECK) ./...

# --- The worker ---------------------------------------------------------------

# The worker is a Compose service built from deploy/Dockerfile, so a code
# change is a rebuild rather than a Ctrl-C: --build recompiles, `up -d`
# recreates the container.
worker: ## Rebuild and restart the worker with the current code
	$(COMPOSE) up -d --build worker

# `compose stop` is an explicit stop, so restart: unless-stopped leaves it
# down until `make worker` brings it back. Handy for watching what callers see
# with nobody polling the task queue.
worker-stop: ## Stop the worker (leaves the rest of the stack up)
	$(COMPOSE) stop worker

# --- Driving the workflow -----------------------------------------------------

# These targets drive the whole workflow from the `temporal` CLI -- no
# application server needed. ID=<customer id> selects the customer.
#
# Addressed as temporal:7233, not localhost: the frontend binds the
# container's own interface. The namespace comes from the container's
# environment.
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

deactivate: ## Soft-leave the program (make deactivate ID=c-001)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name deactivate

reactivate: ## Re-enroll and restore points (make reactivate ID=c-001)
	@$(TCLI) workflow update execute \
	  --workflow-id customer-$(ID) \
	  --name reactivate

# The visibility store holds one document per Run, so a customer who has
# continued-as-new appears once per generation; excluding ContinuedAsNew
# leaves exactly the current one. The same query works in the Temporal UI.
LIST_SCOPE = WorkflowType = 'CustomerRewardsWorkflow' AND ExecutionStatus != 'ContinuedAsNew'

list: ## List customers (make list Q="RewardsLevel = 'gold'")
	@$(TCLI) workflow list --query "$(LIST_SCOPE)$(if $(Q), AND ($(Q)))"

# The audit trail is the Event History Temporal already keeps: nothing stores
# a point-add log, yet every add, rejection and membership change is here.
history: ## Show a customer's Event History (make history ID=c-001)
	@$(TCLI) workflow show --workflow-id customer-$(ID)
