# Three ways to run this service:
#
#   dev    - on your machine, against a local SQLite file      (make dev)
#   stage  - service container + Postgres container, wired up  (make stage-up)
#   prod   - the service built into a container image          (make prod-build)
#
# `make help` lists every target.
#
# On Windows without make, scripts/run.ps1 has the same commands
# (`.\scripts\run.ps1 help`), and .vscode/tasks.json runs them from VS Code.

BINARY  := customersvc
MODULE  := github.com/thinkovation/service_example
PKG     := ./...

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.BuildDate=$(BUILD_DATE)

IMAGE     ?= customersvc
HTTP_PORT ?= 8080

# dev: SQLite on the local filesystem, human-readable logs. These are defaults:
# a .env file, if you keep one, is sourced afterwards and wins.
DEV_ENV := SERVICE_ENV=development; \
	STORE_DRIVER=sqlite; \
	SQLITE_DSN=file:./data/customers.db; \
	HTTP_ADDR=:$(HTTP_PORT); \
	LOG_FORMAT=text; \
	LOG_LEVEL=debug
# set -a exports every assignment, including the ones .env makes.
DEV_SHELL = set -a; $(DEV_ENV); [ -f .env ] && . ./.env; set +a;

COMPOSE       := docker compose
STAGE_COMPOSE := $(COMPOSE) -f deploy/compose.stage.yaml
PROD_COMPOSE  := $(COMPOSE) -f deploy/compose.prod.yaml
# Build stamps and the image tag reach compose through the environment.
COMPOSE_ENV   := IMAGE=$(IMAGE) VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(BUILD_DATE) HTTP_PORT=$(HTTP_PORT)

# The Postgres the test suite uses; `make stage-up` serves it by default.
PG_DSN ?= postgres://customers:customers@localhost:5432/customers?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@awk 'BEGIN {FS = ":.*?## "} \
		/^## /       {printf "\n\033[1m%s\033[0m\n", substr($$0, 4)} \
		/^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo

## Dev - local machine, local SQLite file
.PHONY: dev
dev: ## Run the service locally against ./data/customers.db
	@$(DEV_SHELL) exec go run ./cmd/customersvc

.PHONY: dev-build
dev-build: ## Build the binary into ./bin for local use
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/customersvc

.PHONY: dev-run
dev-run: dev-build ## Build, then run the binary against ./data/customers.db
	@$(DEV_SHELL) exec ./bin/$(BINARY)

.PHONY: dev-reset
dev-reset: ## Delete the local SQLite database
	rm -f data/customers.db data/customers.db-wal data/customers.db-shm

.PHONY: dev-smoke
dev-smoke: ## Check a locally running dev service answers on every endpoint
	go run ./cmd/smoke http://localhost:$(HTTP_PORT)

## Stage - service container plus Postgres container
.PHONY: stage-up
stage-up: ## Build and start the stage stack, then wait for it to be healthy
	$(COMPOSE_ENV) $(STAGE_COMPOSE) up --build --detach --wait
	@echo "stage is up on http://localhost:$(HTTP_PORT)"

.PHONY: stage-down
stage-down: ## Stop the stage stack, keeping the database volume
	$(STAGE_COMPOSE) down

.PHONY: stage-clean
stage-clean: ## Stop the stage stack and delete the database volume
	$(STAGE_COMPOSE) down --volumes

.PHONY: stage-logs
stage-logs: ## Follow the stage service log
	$(STAGE_COMPOSE) logs --follow customersvc

.PHONY: stage-ps
stage-ps: ## Show the stage containers and their health
	$(STAGE_COMPOSE) ps

.PHONY: stage-psql
stage-psql: ## Open psql against the stage database
	$(STAGE_COMPOSE) exec postgres psql -U customers -d customers

.PHONY: stage-smoke
stage-smoke: ## Check the stage stack answers on every endpoint
	go run ./cmd/smoke http://localhost:$(HTTP_PORT)

.PHONY: stage-config
stage-config: ## Render the stage compose file with variables resolved
	$(COMPOSE_ENV) $(STAGE_COMPOSE) config

## Prod - the service as a container image
.PHONY: prod-build
prod-build: ## Build the production image, tagged with the version and latest
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--tag $(IMAGE):$(VERSION) \
		--tag $(IMAGE):latest \
		.

.PHONY: prod-up
prod-up: ## Run the production image against deploy/prod.env
	@test -f deploy/prod.env || { echo "deploy/prod.env is missing; copy deploy/prod.env.example"; exit 1; }
	$(COMPOSE_ENV) $(PROD_COMPOSE) up --detach --wait

.PHONY: prod-down
prod-down: ## Stop the production container
	$(PROD_COMPOSE) down

.PHONY: prod-push
prod-push: ## Push the production image (set REGISTRY, e.g. ghcr.io/thinkovation)
	@test -n "$(REGISTRY)" || { echo "set REGISTRY, e.g. make prod-push REGISTRY=ghcr.io/thinkovation"; exit 1; }
	docker tag $(IMAGE):$(VERSION) $(REGISTRY)/$(IMAGE):$(VERSION)
	docker push $(REGISTRY)/$(IMAGE):$(VERSION)

## Quality
.PHONY: test
test: ## Run the tests (Postgres suites skip unless TEST_POSTGRES_DSN is set)
	go test -race -count=1 $(PKG)

.PHONY: test-postgres
test-postgres: ## Run the tests against SQLite and Postgres (needs make stage-up)
	TEST_POSTGRES_DSN='$(PG_DSN)' go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Report test coverage
	go test -count=1 -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the source
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: tidy
tidy: ## Tidy the module files
	go mod tidy

.PHONY: clean
clean: ## Remove build output, coverage and the local database
	rm -rf bin coverage.out data
