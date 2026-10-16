SHELL := /bin/bash
TOOLS := go tool -modfile=tools/go.mod
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= go-high-load-wallet:$(VERSION)
# actionlint runs from its own module: its YAML dependency conflicts with
# golangci-lint's inside tools/go.mod.
ACTIONLINT_VERSION := v1.7.12

.DEFAULT_GOAL := help
.PHONY: help up up-obs down clean test test-short lint lint-ci vuln build image

help: ## List the available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

up: ## Start the default stack (Postgres, Keycloak, LocalStack, API)
	VERSION=$(VERSION) docker compose up -d --build

up-obs: ## Start the stack plus Prometheus, Grafana, Jaeger and the PostgreSQL exporter, with tracing on
	OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318 VERSION=$(VERSION) docker compose --profile obs up -d --build

down: ## Stop the stack, keeping data volumes
	docker compose --profile obs down

clean: ## Stop the stack and delete its data volumes
	docker compose --profile obs down -v

test: ## Run every test, integration included (needs Docker or DATABASE_URL)
	go test -race -count=1 ./...

test-short: ## Run unit tests only (no Docker)
	go test -short ./...

lint: ## Run golangci-lint
	$(TOOLS) golangci-lint run ./...

lint-ci: ## Validate the GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

vuln: ## Check dependencies for known vulnerabilities
	$(TOOLS) govulncheck ./...

build: ## Build the API binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/Ricardoleaom/go-high-load-wallet/internal/buildinfo.Version=$(VERSION)" -o bin/api ./cmd/api

image: ## Build the API container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .
