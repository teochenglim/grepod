.DEFAULT_GOAL := help

BINARY_NAME := grepod
IMAGE       := $(BINARY_NAME):local
GHCR_IMAGE  := ghcr.io/teochenglim/$(BINARY_NAME)
NAMESPACE   := default
RELEASE     := grepod

# Read the current version from the VERSION file (no external tooling required).
VERSION_CURRENT := $(shell cat VERSION 2>/dev/null || echo 0.0.0)

.PHONY: help
help: ## Show this menu
	@echo "grepod $(VERSION_CURRENT) - available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Release cycle:"
	@echo "  make release VERSION=x.y.z   # bump VERSION, commit, tag, push -> triggers GitHub Actions"

## --- develop ---------------------------------------------------------------

.PHONY: build
build: ## Build the grepod binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY_NAME) ./cmd/server

.PHONY: run
run: ## Run grepod locally (needs a working kubeconfig context)
	go run ./cmd/server

.PHONY: test
test: ## Run the test suite with race detector and coverage
	go test ./... -race -cover

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -l -w .

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/

## --- packaging ---------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the grepod Docker image
	docker build -t $(IMAGE) .

.PHONY: k8s-apply
k8s-apply: ## Apply the k8s/ manifests via Kustomize to the current kubectl context
	kubectl apply -k k8s/

.PHONY: k8s-delete
k8s-delete: ## Delete the k8s/ manifests via Kustomize from the current kubectl context
	kubectl delete -k k8s/

.PHONY: k8s-logs
k8s-logs: ## Tail logs from the grepod deployment in k8s
	kubectl -n $(NAMESPACE) logs -f deployment/$(RELEASE)

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint helm/

.PHONY: helm-template
helm-template: ## Render the Helm chart locally
	helm template $(RELEASE) helm/

.PHONY: helm-install
helm-install: ## Install/upgrade the Helm release
	helm upgrade --install $(RELEASE) helm/ --namespace $(NAMESPACE) --create-namespace

## --- supply-chain hardening -------------------------------------------------

.PHONY: github-action-bump
github-action-bump: ## Pin .github/workflows/*.yml actions to latest release, full commit SHA (uses pinact)
	@# Unauthenticated GitHub API calls are capped at 60/hour and this touches
	@# several actions x (list tags + verify); export GITHUB_TOKEN to raise that limit.
	go run github.com/suzuki-shunsuke/pinact/cmd/pinact@latest run --update
	go run github.com/suzuki-shunsuke/pinact/cmd/pinact@latest run --verify
	@echo "Actions bumped and verified. Review the diff, then run 'make vet test' before committing."

## --- release ------------------------------------------------------------------

.PHONY: version
version: ## Print the version currently in VERSION
	@echo $(VERSION_CURRENT)

.PHONY: bump
bump: ## Rewrite the VERSION file (VERSION=x.y.z required)
	@if [ -z "$(VERSION)" ]; then echo "Usage: make bump VERSION=x.y.z"; exit 1; fi
	@echo "$(VERSION)" > VERSION
	@echo "VERSION -> $(VERSION)"

.PHONY: tag
tag: ## Create and push a git tag for the current VERSION
	git tag v$(VERSION_CURRENT)
	git push origin v$(VERSION_CURRENT)
	@echo "Tagged and pushed v$(VERSION_CURRENT)"

.PHONY: release
release: ## Bump, commit, tag, push - triggers GitHub Actions (VERSION=x.y.z required)
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release VERSION=x.y.z"; exit 1; fi
	$(MAKE) bump VERSION=$(VERSION)
	git add VERSION
	git diff --cached --quiet -- VERSION || git commit -m "release: v$(VERSION)"
	git tag v$(VERSION)
	git push origin HEAD
	git push origin v$(VERSION)
	@echo "Released v$(VERSION) - GitHub Actions will build and publish."
