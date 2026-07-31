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

.PHONY: help up down destroy bootstrap logs ps psql es tools verify-config reap

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
