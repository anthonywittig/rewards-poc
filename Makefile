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

.PHONY: help up down destroy bootstrap logs ps psql es tools verify-config reap \
        worker test enroll status add deactivate

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

psql: $(ENV) ## psql into the Temporal persistence database
	$(COMPOSE) exec postgres psql -U temporal -d temporal

es: $(ENV) ## Elasticsearch index summary
	@$(TCTL) curl -s "http://elasticsearch:9200/_cat/indices/temporal_visibility*?v&h=index,docs.count,store.size"

verify-config: $(ENV) ## Check the platform behaviour the plan depends on
	@$(TCTL) bash /inspect/verify-config.sh

reap: $(ENV) ## Delete closed executions now (make reap WF=customer-x)
	@$(TCTL) env TEMPORAL_NAMESPACE=$(NAMESPACE) WF="$(WF)" bash /reap.sh

# --- Workflow (Phase 1) -----------------------------------------------------

test: ## Run the Go unit tests
	go test ./...

worker: $(ENV) ## Run the workflow worker in the foreground (Ctrl-C to stop)
	TEMPORAL_HOSTPORT=localhost:$(GRPC_PORT) TEMPORAL_NAMESPACE=$(NAMESPACE) \
	  go run ./cmd/worker

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
