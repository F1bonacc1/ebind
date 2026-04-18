.DEFAULT_GOAL := help

GO ?= go
GOLANGCI_LINT ?= golangci-lint
PKGS = ./...

# ---------------------------------------------------------------------------
# Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_.-]+:.*?## / {printf "  \033[1m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort

# ---------------------------------------------------------------------------
# Build / test / lint

.PHONY: build
build: ## Compile every package
	$(GO) build $(PKGS)

.PHONY: test
test: ## Run all tests with race detector
	$(GO) test -race -timeout 180s $(PKGS)

.PHONY: test-short
test-short: ## Run only short/unit tests (no embedded NATS spin-up)
	$(GO) test -race -short -timeout 60s $(PKGS)

.PHONY: test-v
test-v: ## Run all tests verbosely
	$(GO) test -race -v -timeout 180s $(PKGS)

.PHONY: test-count
test-count: ## Run tests 3x to catch flakes
	$(GO) test -race -count=3 -timeout 300s $(PKGS)

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -run=- -bench=. -benchmem $(PKGS)

.PHONY: cover
cover: ## Generate coverage report + open in browser
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKGS)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

.PHONY: cover-summary
cover-summary: ## Print per-package coverage
	$(GO) test -race -cover $(PKGS)

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run $(PKGS)

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix
	$(GOLANGCI_LINT) run --fix $(PKGS)

.PHONY: fmt
fmt: ## Format code
	$(GO) fmt $(PKGS)
	$(GO) tool goimports -w .

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

# ---------------------------------------------------------------------------
# Demo

.PHONY: demo
demo: ## Run the single-process end-to-end demo
	$(GO) run ./cmd/demo

# ---------------------------------------------------------------------------
# Maintenance

.PHONY: clean
clean: ## Remove build + coverage artifacts
	rm -rf ./bin ./dist coverage.out coverage.html

.PHONY: ci
ci: vet lint test ## Run the full CI pipeline locally
