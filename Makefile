SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

BIN        := bin/drswitch
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
CHART      := charts/service
KUBECONFORM_FLAGS := -strict -summary -ignore-missing-schemas \
	-schema-location default \
	-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

.PHONY: help
help: ## Show targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Go
.PHONY: build
build: ## Build bin/drswitch
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/drswitch

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: lint
lint: ## golangci-lint (v2)
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w cmd internal

##@ Manifests
.PHONY: helm-lint
helm-lint: ## helm lint the shared chart against every app's values
	@for app in apps/*/; do \
	  app=$$(basename $$app); \
	  for env in local dev stage prod; do \
	    args="-f apps/$$app/values.yaml"; \
	    [[ -f apps/$$app/values-$$env.yaml ]] && args="$$args -f apps/$$app/values-$$env.yaml"; \
	    echo "helm lint $$app ($$env)"; \
	    helm lint --strict $(CHART) $$args -f dr/active-site.yaml --set site.env=$$env --set site.role=primary >/dev/null || exit 1; \
	  done; \
	done

.PHONY: render
render: ## Render all apps x targets x flag values to stdout
	@hack/render.sh 2>/dev/null

.PHONY: kubeconform
kubeconform: ## Validate rendered charts and ArgoCD manifests with kubeconform
	hack/render.sh 2>/dev/null | kubeconform $(KUBECONFORM_FLAGS) -
	kubeconform $(KUBECONFORM_FLAGS) bootstrap/root-app.yaml bootstrap/clusters/*.yaml applicationsets/

.PHONY: yamllint
yamllint: ## Lint all plain YAML files
	yamllint --strict .

.PHONY: ci
ci: vet lint test helm-lint kubeconform yamllint ## Everything CI runs

##@ Local playground (kind)
.PHONY: set-repo
set-repo: ## Point ArgoCD manifests at your fork: make set-repo REPO_URL=...
	@test -n "$(REPO_URL)" || { echo "usage: make set-repo REPO_URL=https://github.com/<you>/gitops-multicluster-dr.git"; exit 1; }
	hack/set-repo.sh "$(REPO_URL)"

.PHONY: kind-up
kind-up: ## Create two kind clusters, install ArgoCD, register clusters
	hack/kind-up.sh

.PHONY: kind-down
kind-down: ## Delete the kind clusters
	hack/kind-down.sh

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out
