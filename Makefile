.DEFAULT_GOAL := help

BINARY_NAME := grepod
VERSION     := $(shell cat VERSION)
IMAGE       := $(BINARY_NAME)
GHCR_IMAGE  := ghcr.io/teochenglim/$(BINARY_NAME)
NAMESPACE   := default
RELEASE     := grepod

# ---- develop ----------------------------------------------------------

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
	gofmt -s -w .

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/

# ---- packaging ----------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the Docker image, tagged with VERSION and latest
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: k8s-apply
k8s-apply: ## Apply the plain manifests in ./k8s
	kubectl apply -f k8s/

.PHONY: k8s-delete
k8s-delete: ## Delete the plain manifests in ./k8s
	kubectl delete -f k8s/

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint helm/

.PHONY: helm-template
helm-template: ## Render the Helm chart locally
	helm template $(RELEASE) helm/

.PHONY: helm-install
helm-install: ## Install/upgrade the Helm release
	helm upgrade --install $(RELEASE) helm/ --namespace $(NAMESPACE) --create-namespace

# ---- supply-chain ----------------------------------------------------------

.PHONY: github-action-bump
github-action-bump: ## Pin every GitHub Action to a commit SHA via pinact
	pinact run

# ---- release ----------------------------------------------------------

.PHONY: version
version: ## Print the current VERSION
	@cat VERSION

.PHONY: bump
bump: ## Bump VERSION without tagging (usage: make bump VERSION=x.y.z)
	@test -n "$(VERSION)" || (echo "usage: make bump VERSION=x.y.z" >&2; exit 1)
	@echo "$(VERSION)" > VERSION
	@echo "VERSION bumped to $(VERSION)"

.PHONY: tag
tag: ## Tag HEAD with v<VERSION> (usage: make tag VERSION=x.y.z)
	@test -n "$(VERSION)" || (echo "usage: make tag VERSION=x.y.z" >&2; exit 1)
	git tag v$(VERSION)

.PHONY: release
release: ## Bump VERSION, commit, tag, and push (usage: make release VERSION=x.y.z)
	@test -n "$(VERSION)" || (echo "usage: make release VERSION=x.y.z" >&2; exit 1)
	echo "$(VERSION)" > VERSION
	git add VERSION
	git commit -m "release: v$(VERSION)"
	git tag v$(VERSION)
	git push origin HEAD
	git push origin v$(VERSION)

.PHONY: help
help: ## Show this help (default target — just run `make`)
	@echo "grepod — current VERSION: $(VERSION)"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "To cut a release: \033[33mmake release VERSION=x.y.z\033[0m  (bumps VERSION, commits, tags, and pushes — triggers the tag-driven release CI)"
