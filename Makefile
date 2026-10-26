# Requires Docker with Compose v2.17+ (for `up --wait`). Copy .env.example to
# .env and set JWT_SECRET and ADMIN_PASSWORD first.

COMPOSE  ?= docker compose
TOOLS    := $(COMPOSE) --profile tools
API_URL  ?= http://localhost:8080
DURATION ?= 1m

.DEFAULT_GOAL := help
.PHONY: help up down clean logs ps verify verify-local seed simulate test test-unit test-integration

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: ## Build images (multi-stage) and start the stack; waits until every service is healthy
	$(COMPOSE) up -d --build --wait

down: ## Stop the stack (keeps the database volume)
	$(COMPOSE) down

clean: ## Stop the stack and delete the database volume
	$(COMPOSE) down -v --remove-orphans

logs: ## Follow API logs
	$(COMPOSE) logs -f api

ps: ## Show service health
	$(COMPOSE) ps

verify: up ## Test every API requirement (F-1…F-6, T-1…T-4) and print a PASS/FAIL table
	$(TOOLS) run --rm --build simulator verify

verify-local: ## Same checks against an already running API at $(API_URL), using local Go
	cd simulator && go run . verify -api $(API_URL)

seed: up ## Create 50 branches, customers and the display account
	$(TOOLS) run --rm --build simulator seed

simulate: seed ## Drive the 50 branches with tickets, appointments and calls for $(DURATION)
	$(TOOLS) run --rm simulator run -duration $(DURATION)

test: test-unit test-integration ## Run all Go and frontend tests

test-unit: ## Unit tests only (no database needed)
	cd backend && go test -count=1 ./internal/auth/... ./internal/httpx/...
	cd frontend && npm test

test-integration: ## Integration tests in Testcontainers (Docker required; fails instead of skipping without it)
	cd backend && TESTCONTAINERS_REQUIRED=1 go test -count=1 ./internal/...
