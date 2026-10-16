SHELL := /bin/bash
TOOLS := go tool -modfile=tools.mod
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= go-high-load-wallet:$(VERSION)

.DEFAULT_GOAL := help
.PHONY: help up down clean test test-short lint vuln build image

help: ## List the available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

up: ## Start the default stack (Postgres, Keycloak, LocalStack, API)
	VERSION=$(VERSION) docker compose up -d --build

down: ## Stop the stack, keeping data volumes
	docker compose down

clean: ## Stop the stack and delete its data volumes
	docker compose down -v

test: ## Run every test, integration included (needs Docker or DATABASE_URL)
	go test -race -count=1 ./...

test-short: ## Run unit tests only (no Docker)
	go test -short ./...

lint: ## Run golangci-lint
	$(TOOLS) golangci-lint run ./...

vuln: ## Check dependencies for known vulnerabilities
	$(TOOLS) govulncheck ./...

build: ## Build the API binary into bin/
	CGO_ENABLED=0 go build -trimpath -o bin/api ./cmd/api

image: ## Build the API container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .
