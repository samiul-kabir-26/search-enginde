SHELL := /bin/bash
.DEFAULT_GOAL := help

GO_VERSION := 1.27.3
export GOTOOLCHAIN := go$(GO_VERSION)

BIN := bin
export PATH := $(CURDIR)/bin:$(PATH)

GOLANGCI_LINT_VERSION := v2.15.0
SQLC_VERSION          := v1.31.1
MIGRATE_VERSION       := v4.20.1
BUF_VERSION           := v1.75.0
PROTOC_GEN_GO_VERSION := v1.36.12
PROTOC_GEN_GRPC_VERSION := v1.6.2
UV_VERSION            := 0.13.5
ONNXRUNTIME_VERSION   := 1.31.0
REDOCLY_VERSION       := 2.71.0

COMPOSE := docker compose --project-directory . -f deploy/docker-compose.yml --env-file .env
ENV_LOAD := set -a; source .env; set +a;
UV_RUN := uv run --frozen --all-groups

SERVICES := search-api control-api indexer featurejob seed

##@ Setup

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.env:
	cp .env.example .env

.PHONY: setup
setup: .env tools py-setup web-setup onnxruntime ## Install every dev dependency (Go tools, Python venv, web deps, ONNX Runtime)

.PHONY: tools
tools: ## Install pinned Go dev tools into ./bin
	GOBIN="$(CURDIR)/bin" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN="$(CURDIR)/bin" go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	GOBIN="$(CURDIR)/bin" go install -tags 'mysql clickhouse' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
	GOBIN="$(CURDIR)/bin" go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN="$(CURDIR)/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN="$(CURDIR)/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)

$(BIN)/uv:
	mkdir -p $(BIN)
	curl -fsSL https://github.com/astral-sh/uv/releases/download/$(UV_VERSION)/uv-x86_64-unknown-linux-gnu.tar.gz \
		| tar -xz --strip-components=1 -C $(BIN) uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx

.PHONY: py-setup
py-setup: $(BIN)/uv ## Create tools/.venv (Python 3.12) with data, training and model-export deps
	cd tools && uv sync --frozen --all-groups

.PHONY: web-setup
web-setup: ## Install onboarding UI dependencies
	cd web/onboarding && npm ci

.PHONY: onnxruntime
onnxruntime: third_party/onnxruntime/lib/libonnxruntime.so ## Download the ONNX Runtime shared library used by the in-process scorer

third_party/onnxruntime/lib/libonnxruntime.so:
	mkdir -p third_party/onnxruntime
	curl -fsSL https://github.com/microsoft/onnxruntime/releases/download/v$(ONNXRUNTIME_VERSION)/onnxruntime-linux-x64-$(ONNXRUNTIME_VERSION).tgz \
		| tar -xz --strip-components=1 -C third_party/onnxruntime

##@ Local stack

.PHONY: up
up: .env ## Start infrastructure, wait until healthy, create topics and buckets
	$(COMPOSE) up -d --wait --wait-timeout 300
	$(MAKE) --no-print-directory init

.PHONY: init
init: ## Create Kafka topics and S3 buckets (idempotent)
	$(COMPOSE) run --rm redpanda-init
	$(COMPOSE) run --rm s3-init

.PHONY: up-models
up-models: .env ## Start infrastructure plus Triton (large image; needed from M1)
	$(COMPOSE) --profile models up -d --wait --wait-timeout 600
	$(MAKE) --no-print-directory init

.PHONY: up-obs
up-obs: .env ## Start infrastructure plus OTel collector, Jaeger, Prometheus, Grafana
	$(COMPOSE) --profile obs up -d --wait --wait-timeout 300
	$(MAKE) --no-print-directory init

.PHONY: down
down: ## Stop the stack (keeps volumes)
	$(COMPOSE) --profile '*' down

.PHONY: nuke
nuke: ## Stop the stack and delete all volumes
	$(COMPOSE) --profile '*' down -v

.PHONY: ps
ps: ## Show stack status
	$(COMPOSE) ps

.PHONY: logs
logs: ## Tail stack logs
	$(COMPOSE) logs -f --tail=100

##@ Database

.PHONY: migrate
migrate: .env ## Apply MySQL and ClickHouse migrations
	@$(ENV_LOAD) migrate -path db/migrations/mysql -database "$$MYSQL_MIGRATE_URL" up
	@$(ENV_LOAD) migrate -path db/migrations/clickhouse -database "$$CLICKHOUSE_MIGRATE_URL" up

.PHONY: migrate-down
migrate-down: .env ## Roll back the last MySQL migration
	@$(ENV_LOAD) migrate -path db/migrations/mysql -database "$$MYSQL_MIGRATE_URL" down 1

.PHONY: migrate-new
migrate-new: ## Create a migration: make migrate-new DB=mysql NAME=add_x
	migrate create -ext sql -dir db/migrations/$(DB) -seq $(NAME)

##@ Code

.PHONY: gen
gen: ## Generate code: protobuf (KServe v2), sqlc, OpenAPI TypeScript client
	buf generate
	sqlc generate
	@if [ -d web/onboarding/node_modules ]; then cd web/onboarding && npm run gen:api; fi

.PHONY: fmt
fmt: ## Format Go code
	golangci-lint fmt

.PHONY: lint
lint: ## Lint Go code and OpenAPI specs
	golangci-lint run ./...
	npx --yes @redocly/cli@$(REDOCLY_VERSION) lint --config api/openapi/redocly.yaml

.PHONY: build
build: ## Build all Go binaries into ./bin
	@for s in $(SERVICES); do go build -o "$(BIN)/$$s" ./cmd/$$s || exit 1; done

.PHONY: test
test: ## Run unit tests
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration tests (testcontainers; needs Docker)
	go test -race -count=1 -tags integration ./...

.PHONY: cover
cover: ## Coverage for the packages with an 80% target (unit + integration tests)
	go test -p 1 -tags integration -count=1 -coverprofile=coverage.out \
		-coverpkg=$$(go list ./internal/pipeline/... ./internal/stages/... ./internal/tenant/... ./internal/security/... | grep -v -E 'stagestest|tenantdb' | paste -sd, -) \
		./internal/pipeline/... ./internal/stages/... ./internal/tenant/... ./internal/security/... ./internal/searchapi/... ./internal/e2e/...
	@go tool cover -func=coverage.out | tail -1

##@ Run

.PHONY: run-search-api
run-search-api: .env ## Run search-api locally against the Compose stack
	@$(ENV_LOAD) go run ./cmd/search-api

.PHONY: run-control-api
run-control-api: .env ## Run control-api locally against the Compose stack
	@$(ENV_LOAD) go run ./cmd/control-api

.PHONY: run-indexer
run-indexer: .env ## Run the streaming indexer (consumer group) against the Compose stack
	@$(ENV_LOAD) go run ./cmd/indexer

.PHONY: run-web
run-web: ## Run the onboarding wizard and admin console (Next.js) on :3000
	cd web/onboarding && npm run dev

.PHONY: e2e
e2e: ## Playwright onboarding and admin E2E (needs the stack, run-* services and `make seed`; `npx playwright install chromium` once)
	cd web/onboarding && npm run test:e2e

.PHONY: centroids
centroids: .env ## Rebuild category centroids for a tenant's active index (TENANT=demo)
	@$(ENV_LOAD) go run ./cmd/featurejob centroids -tenant $(or $(TENANT),demo)

.PHONY: run-featurejob
run-featurejob: .env ## Run featurejob on its schedule for every live tenant (online features, centroids)
	@$(ENV_LOAD) go run ./cmd/featurejob

.PHONY: features
features: .env ## Refresh online features once from ClickHouse events (TENANT=demo)
	@$(ENV_LOAD) go run ./cmd/featurejob -once -tenant $(or $(TENANT),demo)

.PHONY: token
token: .env ## Print a JWT for the demo API key (needs `make run-control-api`)
	@curl -fsS -X POST http://localhost:8081/v1/auth/token -H 'Content-Type: application/json' \
		-d "{\"api_key\":\"$$(cat data/demo_api_key)\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'

.PHONY: backfill
backfill: .env ## Rebuild a tenant index from the catalog topic and swap aliases (TENANT=demo)
	@$(ENV_LOAD) go run ./cmd/indexer -backfill -tenant $(or $(TENANT),demo)

##@ Data, eval and load

.PHONY: data
data: ## Download ABO metadata and the selected images, then build data/ (deterministic, seed 42)
	cd tools && $(UV_RUN) python -m dataprep.select --seed 42
	cd tools && $(UV_RUN) python -m dataprep.synthesize --seed 42
	cd tools && $(UV_RUN) python -m dataprep.simulate --seed 42

.PHONY: models
models: ## Export CLIP and BGE to ONNX into the Triton model repository (parity-checked)
	cd tools && $(UV_RUN) python -m models.export

.PHONY: models-smoke
models-smoke: ## Smoke-test the Triton embedders (needs `make up-models`)
	cd tools && $(UV_RUN) python -m models.smoke

.PHONY: seed
seed: .env ## Create the demo tenant and publish the catalog and vendor events (M1, M6)
	@$(ENV_LOAD) go run ./cmd/seed

.PHONY: seed-events
seed-events: .env ## Publish the simulated interaction history (data/events) to the events topic (M6)
	@$(ENV_LOAD) go run ./cmd/seed -events

.PHONY: train
train: .env ## Learned ranking (M7): dataset from logged events, LightGBM prerank + rank, eval report, publish (TENANT=demo)
	@$(ENV_LOAD) go run ./cmd/featurejob dataset -tenant $(or $(TENANT),demo) -out data/training/$(or $(TENANT),demo).csv.gz
	cd tools && $(UV_RUN) python -m training.train --data ../data/training/$(or $(TENANT),demo).csv.gz
	@$(ENV_LOAD) go run ./cmd/featurejob publish -dir tools/training/out
	@echo "Restart Triton to load rank_gbdt_v1: docker compose --project-directory . -f deploy/docker-compose.yml restart triton"

.PHONY: eval
eval: ## Run offline eval suites against the running stack (M10)
	cd tools && $(UV_RUN) python -m eval.run

.PHONY: loadtest
loadtest: ## Run the k6 text search load test at 50 RPS (needs search-api and control-api running)
	SEARCH_TOKEN=$$($(MAKE) -s token) k6 run loadtest/search_text.js
