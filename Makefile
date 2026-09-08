.DEFAULT_GOAL := help

GO ?= go

.PHONY: help setup format generate lint vet vuln test test-race test-contract test-e2e test-chaos test-chaos-long build check
.PHONY: infra-up infra-down observability-up app-up migrate-up migrate-down run-api run-worker

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "%-22s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Baixa dependencias e gera codigo.
	$(GO) mod download
	$(MAKE) generate

format: ## Formata codigo Go.
	$(GO) fmt ./...

generate: ## Gera strict server OpenAPI e queries sqlc.
	$(GO) tool oapi-codegen --config api/oapi-codegen.yaml api/openapi.yaml
	$(GO) tool sqlc generate --file db/sqlc.yaml

lint: ## Executa golangci-lint.
	$(GO) tool golangci-lint run

vet: ## Executa analise estatica da toolchain.
	$(GO) vet ./...

vuln: ## Verifica vulnerabilidades conhecidas.
	$(GO) tool govulncheck ./...

test: ## Executa testes unitarios.
	$(GO) test ./...

test-race: ## Executa testes com detector de race.
	$(GO) test -race ./...

test-contract: ## Valida handlers, exemplos e manifesto contra o OpenAPI.
	$(GO) test -count=1 ./internal/contracttest ./internal/transport/http

test-e2e: ## Executa fluxos ponta a ponta (requer ambiente E2E isolado).
	$(GO) test -race -tags=e2e -count=1 -timeout=5m ./tests/e2e

test-chaos: ## Executa falhas deterministicas e o proxy de rede.
	$(GO) test -race -count=1 ./tests/chaos ./internal/application/outbox ./internal/application/consumer ./internal/adapters/rabbitmq

test-chaos-long: ## Executa recuperacao e backlog com dependencias reais isoladas.
	$(GO) test -tags=chaos -count=1 -timeout=10m -v ./tests/chaos

build: ## Compila todos os comandos.
	$(GO) build ./cmd/...

check: format generate vet lint test-race build ## Executa verificacoes locais do CI.
	git diff --exit-code

infra-up: ## Inicia PostgreSQL e RabbitMQ.
	docker compose up -d postgres rabbitmq

infra-down: ## Encerra a infraestrutura local.
	docker compose down

observability-up: ## Inicia infraestrutura e observabilidade local.
	docker compose --profile observability up -d postgres rabbitmq observability

app-up: ## Constroi e inicia toda a aplicacao em containers.
	docker compose --profile app up --build

migrate-up: ## Aplica todas as migrations.
	$(GO) run ./cmd/migrate up

migrate-down: ## Reverte a migration mais recente.
	$(GO) run ./cmd/migrate down

run-api: ## Executa a API localmente.
	$(GO) run ./cmd/api

run-worker: ## Executa o worker localmente.
	HTTP_ADDRESS=:8081 OTEL_SERVICE_NAME=payments-worker $(GO) run ./cmd/worker
