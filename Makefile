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
# JUNIT_REPORT=<file> additionally writes a JUnit XML report (via gotestsum).
test: fmt vet $(if $(JUNIT_REPORT),gotestsum) ## Run unit tests (everything except test/e2e).
	$(if $(JUNIT_REPORT),$(GOTESTSUM) --junitfile $(abspath $(JUNIT_REPORT)) --format standard-verbose --,go test) \
		$$(go list ./... | grep -v /e2e) -coverprofile cover.out
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

##@ Container images

.PHONY: docker-build
docker-build: ## Build the minter container image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the minter container image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: docker-build-mock
docker-build-mock: ## Build the mock CP4D image used by the e2e suite.
	$(CONTAINER_TOOL) build -f test/mockcpd/Dockerfile -t $(MOCK_IMG) .

##@ Deployment

NAMESPACE ?= mint4v
PULL_POLICY ?= IfNotPresent

.PHONY: deploy
deploy: ## Deploy the chart against IMG (any cluster the kubeconfig points at). Pass VALUES=<file>.
	@IMG='$(IMG)'; helm upgrade --install mint4v charts/mint4v -n $(NAMESPACE) \
		$(if $(VALUES),-f $(VALUES)) \
		--set image.repository="$${IMG%:*}" \
		--set image.tag="$${IMG##*:}" \
		--set image.pullPolicy=$(PULL_POLICY)

OCP_IMG_REPO = image-registry.openshift-image-registry.svc:5000/$(NAMESPACE)/mint4v

.PHONY: ocp-build
ocp-build: ## Build the mint4v image in-cluster via a binary build into NAMESPACE (OpenShift).
	@command -v oc >/dev/null || { echo "oc is not installed"; exit 1; }
	kubectl create namespace $(NAMESPACE) 2>/dev/null || true
	oc get bc mint4v -n $(NAMESPACE) >/dev/null 2>&1 || \
		oc new-build --binary --strategy=docker --name=mint4v -n $(NAMESPACE)
	oc start-build mint4v -n $(NAMESPACE) --from-dir=. --follow

.PHONY: ocp-deploy
ocp-deploy: ocp-build ## Build in-cluster, then deploy against the internal-registry image (OpenShift). Pass VALUES=<file>.
	$(MAKE) deploy IMG=$(OCP_IMG_REPO):latest NAMESPACE=$(NAMESPACE) PULL_POLICY=Always VALUES=$(VALUES)
	kubectl rollout restart deployment/mint4v -n $(NAMESPACE)
	kubectl rollout status deployment/mint4v -n $(NAMESPACE) --timeout=3m

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
# JUNIT_REPORT=<file> additionally writes a JUnit XML report (via ginkgo).
test-e2e: setup-test-e2e ## Run the e2e suite against a KIND cluster, then tear it down.
	KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v \
		$(if $(JUNIT_REPORT),-ginkgo.junit-report=$(abspath $(JUNIT_REPORT))) -timeout=30m
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Delete the e2e KIND cluster.
	kind delete cluster --name $(KIND_CLUSTER)

##@ E2E against an existing cluster (e.g. OpenShift)

E2E_NAMESPACE ?= mint4v-e2e
E2E_IMG ?= image-registry.openshift-image-registry.svc:5000/$(E2E_NAMESPACE)/mint4v:latest
E2E_MOCK_IMG ?= image-registry.openshift-image-registry.svc:5000/$(E2E_NAMESPACE)/mockcpd:latest

.PHONY: ocp-build-e2e
ocp-build-e2e: ## Build both e2e images in-cluster (minter + mock CP4D) into E2E_NAMESPACE.
	$(MAKE) ocp-build NAMESPACE=$(E2E_NAMESPACE)
	oc get bc mockcpd -n $(E2E_NAMESPACE) >/dev/null 2>&1 || { \
		oc new-build --binary --strategy=docker --name=mockcpd -n $(E2E_NAMESPACE) && \
		oc patch bc mockcpd -n $(E2E_NAMESPACE) --type=merge \
			-p '{"spec":{"strategy":{"dockerStrategy":{"dockerfilePath":"test/mockcpd/Dockerfile"}}}}'; }
	oc start-build mockcpd -n $(E2E_NAMESPACE) --from-dir=. --follow

.PHONY: test-e2e-external
test-e2e-external: ## Run the e2e suite against the current kubeconfig cluster and an external Vault.
	@test -n "$(E2E_VAULT_ADDR)" || { echo "E2E_VAULT_ADDR is required"; exit 1; }
	@test -n "$(E2E_VAULT_TOKEN)" || { echo "E2E_VAULT_TOKEN is required"; exit 1; }
	@test -n "$(E2E_KUBERNETES_HOST)" || { echo "E2E_KUBERNETES_HOST is required"; exit 1; }
# @-silenced: the recipe carries E2E_VAULT_TOKEN, keep it out of make's echo.
	@E2E_EXTERNAL=true E2E_NAMESPACE=$(E2E_NAMESPACE) \
		E2E_IMG=$(E2E_IMG) E2E_MOCK_IMG=$(E2E_MOCK_IMG) \
		E2E_VAULT_ADDR=$(E2E_VAULT_ADDR) E2E_VAULT_TOKEN=$(E2E_VAULT_TOKEN) \
		E2E_VAULT_NAMESPACE=$(E2E_VAULT_NAMESPACE) E2E_KUBERNETES_HOST=$(E2E_KUBERNETES_HOST) \
		E2E_KEEP=$(E2E_KEEP) \
		go test ./test/e2e/ -v -ginkgo.v -timeout=30m

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

GOTESTSUM = $(LOCALBIN)/gotestsum
GOTESTSUM_VERSION ?= v1.13.0

.PHONY: gotestsum
gotestsum: $(GOTESTSUM)
$(GOTESTSUM): $(LOCALBIN)
	test -s $(GOTESTSUM) || GOBIN=$(LOCALBIN) go install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

##@ Help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
