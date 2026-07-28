# Image URL to use for all building/pushing image targets
IMG ?= mint4v:dev
MOCK_IMG ?= mockcpd:e2e
CONTAINER_TOOL ?= docker

KIND_CLUSTER ?= vault-token-minter-e2e

.PHONY: all
all: build

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: build
build: fmt vet ## Build the minter binary.
	go build -o bin/mint4v ./cmd

.PHONY: test
test: fmt vet ## Run unit tests (everything except test/e2e).
	go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out
	go tool cover -func=cover.out | tail -1

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	helm lint charts/mint4v

##@ Build

.PHONY: docker-build
docker-build: ## Build the minter container image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-build-mock
docker-build-mock: ## Build the mock CP4D image used by the e2e suite.
	$(CONTAINER_TOOL) build -f test/mockcpd/Dockerfile -t $(MOCK_IMG) .

##@ E2E

.PHONY: setup-test-e2e
setup-test-e2e: ## Create the KIND cluster if it does not already exist.
	@command -v kind >/dev/null || { echo "kind is not installed"; exit 1; }
	@command -v helm >/dev/null || { echo "helm is not installed"; exit 1; }
	@case "$$(kind get clusters)" in \
		*"$(KIND_CLUSTER)"*) echo "kind cluster $(KIND_CLUSTER) exists" ;; \
		*) kind create cluster --name $(KIND_CLUSTER) --wait 120s ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e ## Run the e2e suite against a KIND cluster, then tear it down.
	KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -timeout=30m
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Delete the e2e KIND cluster.
	kind delete cluster --name $(KIND_CLUSTER)

##@ Tooling

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) version | grep -q $(GOLANGCI_LINT_VERSION:v%=%) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
