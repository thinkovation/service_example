BINARY      := customersvc
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE      := github.com/thinkovation/service_example
LDFLAGS     := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.BuildDate=$(BUILD_DATE)

# A throwaway Postgres for `make postgres-up` and the Postgres test suite.
PG_CONTAINER ?= customersvc-postgres
PG_DSN       ?= postgres://customers:customers@localhost:5432/customers?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the service binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/customersvc

.PHONY: run
run: ## Run the service against SQLite (development default)
	go run ./cmd/customersvc

.PHONY: run-postgres
run-postgres: ## Run the service against Postgres (see PG_DSN)
	STORE_DRIVER=postgres DATABASE_URL='$(PG_DSN)' go run ./cmd/customersvc

.PHONY: test
test: ## Run the test suite (Postgres tests are skipped unless TEST_POSTGRES_DSN is set)
	go test -race -count=1 $(PKG)

.PHONY: test-postgres
test-postgres: ## Run the test suite against SQLite and Postgres
	TEST_POSTGRES_DSN='$(PG_DSN)' go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Run the tests and open a coverage summary
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

.PHONY: postgres-up
postgres-up: ## Start a disposable Postgres in Docker
	docker run -d --rm --name $(PG_CONTAINER) \
		-e POSTGRES_USER=customers -e POSTGRES_PASSWORD=customers -e POSTGRES_DB=customers \
		-p 5432:5432 postgres:16-alpine

.PHONY: postgres-down
postgres-down: ## Stop the disposable Postgres
	docker stop $(PG_CONTAINER)

.PHONY: docker-build
docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(BINARY):$(VERSION) .

.PHONY: clean
clean: ## Remove build output and the development database
	rm -rf bin coverage.out data
