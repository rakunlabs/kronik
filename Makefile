PROJECT  := kronik
MAIN_PKG := ./cmd/$(PROJECT)

BUILD_DATE   := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
BUILD_COMMIT := $(shell git rev-parse --short HEAD 2> /dev/null || echo unknown)
VERSION      := $(or $(IMAGE_TAG),$(shell git describe --tags --first-parent --match "v*" 2> /dev/null || echo v0.0.0))
LDFLAGS      := -X github.com/rakunlabs/kronik/internal/config.Version=$(VERSION) -X main.commit=$(BUILD_COMMIT) -X main.date=$(BUILD_DATE)

LOCAL_DSN := postgres://kronik:kronik@localhost:5432/kronik?sslmode=disable

GOOS   ?= linux
GOARCH ?= amd64

.DEFAULT_GOAL := help

.PHONY: install-ui
install-ui: ## Install UI dependencies
	@cd _ui && pnpm install --frozen-lockfile

.PHONY: build-ui
build-ui: install-ui ## Build the UI into internal/server/dist
	@cd _ui && pnpm run build

.PHONY: build
build: build-ui ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w $(LDFLAGS)" -o bin/$(PROJECT) $(MAIN_PKG)

.PHONY: build-container
build-container: build-ui ## Build the container image with the test tag
	GOOS=$(GOOS) GOARCH=$(GOARCH) goreleaser build --snapshot --clean --single-target
	docker build --platform=$(GOOS)/$(GOARCH) --build-arg TARGETPLATFORM=. \
		-t $(PROJECT):test -f ci/Dockerfile dist/$(PROJECT)_$(GOOS)_$(GOARCH)*/

.PHONY: snapshot
snapshot: build-ui ## Full snapshot release: every archive and the image, nothing published
	goreleaser release --snapshot --clean

.PHONY: env
env: ## Start a local PostgreSQL
	docker compose -p $(PROJECT) -f env/docker-compose.yaml up -d --wait

.PHONY: env-down
env-down: ## Stop the local PostgreSQL
	docker compose -p $(PROJECT) -f env/docker-compose.yaml down -v

.PHONY: run
run: ## Run the backend on :8080 against the local PostgreSQL
	KRONIK_STORE_DSN='$(LOCAL_DSN)' go run -ldflags="$(LDFLAGS)" $(MAIN_PKG)

.PHONY: run-ui
run-ui: ## Run the Vite dev server on :3000 (proxies /api to :8080)
	@cd _ui && pnpm run dev

.PHONY: test
test: ## Run Go tests (store tests need `make env`)
	KRONIK_TEST_DSN='$(LOCAL_DSN)' go test -race ./...

.PHONY: lint
lint: ## Lint Go code and type-check the UI
	@golangci-lint run ./...
	@cd _ui && pnpm run check

.PHONY: help
help: ## Show this help
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'
