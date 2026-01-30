PROJECT ?= purelb
REPO ?= ko.local

PREFIX ?= ${PROJECT}
REGISTRY_IMAGE ?= ${REPO}/${PREFIX}
SUFFIX ?= v0.0.0-dev
MANIFEST_SUFFIX ?= ${SUFFIX}
COMMANDS = $(shell find cmd -maxdepth 1 -mindepth 1 -type d)
NETBOX_USER_TOKEN = no-op
NETBOX_BASE_URL = http://192.168.1.40:30080/
CRDS = deployments/crds/purelb.io_lbnodeagents.yaml deployments/crds/purelb.io_servicegroups.yaml

# Tools that we use.
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.18.0
KUSTOMIZE = go run sigs.k8s.io/kustomize/kustomize/v4@v4.5.2
HELM = go run helm.sh/helm/v3/cmd/helm@v3.11
HUGO = go run -tags extended github.com/gohugoio/hugo@v0.111.3
KO = go run github.com/google/ko@v0.17.1

##@ Default Goal
.PHONY: help
help: ## Display help message
	@echo "Usage:\n  make <goal> [VAR=value ...]"
	@echo "\nVariables"
	@echo "  PREFIX Docker tag prefix (useful to set the docker registry)"
	@echo "  SUFFIX Docker tag suffix (the part after ':')"
	@awk 'BEGIN {FS = "[:=].*##"}; \
		/^[A-Z]+=.*?##/ { printf "  %-15s %s\n", $$1, $$2 } \
		/^[%a-zA-Z0-9_-]+:.*?##/ { printf "  %-15s %s\n", $$1, $$2 } \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development Goals
all: check crd image ## Build it all!

.PHONY: check
check: generate ## Run "short" tests
	go vet ./...
	NETBOX_BASE_URL=${NETBOX_BASE_URL} NETBOX_USER_TOKEN=${NETBOX_USER_TOKEN} go test -race -short ./...

.PHONY: image
image: generate ## Build executables and containers
	KO_DOCKER_REPO=${REGISTRY_IMAGE} TAG=${SUFFIX} ${KO} build --base-import-paths --tags=${SUFFIX} ./cmd/allocator
	KO_DOCKER_REPO=${REGISTRY_IMAGE} TAG=${SUFFIX} ${KO} build --base-import-paths --tags=${SUFFIX} ./cmd/lbnodeagent

.PHONY: run-%
run-%:  ## Run PureLB command locally (e.g., 'make run-allocator')
	go run ./cmd/$(subst run-,,$@)

.PHONY: clean-gen
clean-gen:  ## Delete generated files
	rm -fr pkg/generated/
	rm -f pkg/apis/purelb/v1/zz_generated.deepcopy.go
	rm -fr deployments/${PROJECT}-*.yaml

.PHONY: generate
generate:  ## Generate client-side stubs for our custom resources
	go mod download
	hack/update-codegen.sh

crd: $(CRDS) ## Generate CRDs from golang api structs
$(CRDS) &: pkg/apis/purelb/v1/*.go
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=deployments/crds

.ONESHELL:
.PHONY: manifest
manifest: CACHE != mktemp
manifest:  ## Generate deployment manifest
	cd deployments/samples
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX}
	$(KUSTOMIZE) build . > ../${PROJECT}-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.PHONY: helm
helm:  ## Package PureLB using Helm
	rm -rf build/build
	mkdir -p build/build
	cp -r build/helm/purelb build/build/
	cp deployments/crds/purelb.io_*.yaml build/build/purelb/crds
	cp README.md build/build/purelb

	sed \
	--expression="s~DEFAULT_REPO~${REGISTRY_IMAGE}~" \
	--expression="s~DEFAULT_TAG~${SUFFIX}~" \
	build/helm/purelb/values.yaml > build/build/purelb/values.yaml

	${HELM} package \
	--version "${SUFFIX}" --app-version "${SUFFIX}" \
	build/build/purelb

.PHONY: scan
scan: ## Scan for vulnerabilities using govulncheck
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: website
website: ## Generate documentation website
	${HUGO} --source website
