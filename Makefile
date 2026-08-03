# Rewards POC. Every stack setting lives in deploy/docker-compose.yml,
# written out literally -- there is no env file.

COMPOSE := docker compose -f deploy/docker-compose.yml

# Host-side ports, repeated from docker-compose.yml for the banner.
API_PORT = 8081
WEB_PORT = 5173
TEMPORAL_UI_PORT = 8080

.PHONY: help up destroy logs ps

# The temporal CLI ships in the server image; exec-ing into it beats a
# separate container by several seconds per invocation.
TCTL = $(COMPOSE) exec -T temporal

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

up: ## Start the whole stack: bootstrap, worker, API, UI, demo customers
	$(COMPOSE) up -d --wait postgres elasticsearch temporal temporal-ui
	@$(TCTL) bash /bootstrap.sh
# After bootstrap, so neither talks to a namespace that doesn't exist yet.
	$(COMPOSE) up -d --build --wait worker api
	$(COMPOSE) run --rm -T --build seed
# The UI starts last and is not waited on: a first start installs npm
# packages for minutes, and nothing else needs it. `make logs SVC=web` shows
# progress.
	$(COMPOSE) up -d web
	@echo
	@echo "React UI:     http://localhost:$(WEB_PORT) (starting -- make logs SVC=web)"
	@echo "Temporal UI:  http://localhost:$(TEMPORAL_UI_PORT)"
	@echo "HTTP API:     http://localhost:$(API_PORT)/api/customers"

destroy: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

logs: ## Tail logs (make logs SVC=temporal)
	$(COMPOSE) logs -f $(SVC)

ps: ## Show stack status
	$(COMPOSE) ps
