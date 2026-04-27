PROJECT ?= purelb
REPO ?= ko.local

PREFIX ?= ${PROJECT}
REGISTRY_IMAGE ?= ${REPO}/${PREFIX}
SUFFIX ?= v0.0.0-dev
MANIFEST_SUFFIX ?= ${SUFFIX}
COMMANDS = $(shell find cmd -maxdepth 1 -mindepth 1 -type d)
NETBOX_USER_TOKEN = no-op
NETBOX_BASE_URL = http://192.168.1.40:30080/
GOBGP_IMAGE     ?= ghcr.io/purelb/k8gobgp
GOBGP_TAG       ?= v0.2.2
GOBGP_IMAGE_TAG ?= 0.2.2
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

.PHONY: plugin
plugin: ## Build kubectl-purelb plugin binary
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(SUFFIX) -X main.commit=$(shell git rev-parse --short HEAD)" -o kubectl-purelb ./cmd/kubectl-purelb

.PHONY: run-%
run-%:  ## Run PureLB command locally (e.g., 'make run-allocator')
	go run ./cmd/$(subst run-,,$@)

.PHONY: clean-gen
clean-gen:  ## Delete generated files
	rm -fr pkg/generated/
	rm -f pkg/apis/purelb/v2/zz_generated.deepcopy.go
	rm -fr deployments/${PROJECT}-*.yaml

.PHONY: generate
generate:  ## Generate client-side stubs for our custom resources
	go mod download
	hack/update-codegen.sh

crd: $(CRDS) ## Generate CRDs from golang api structs
$(CRDS) &: pkg/apis/purelb/v2/*.go
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=deployments/crds

.ONESHELL:
.PHONY: manifest
manifest: CACHE != mktemp
manifest:  ## Generate deployment manifest (with samples and k8gobgp)
	cd deployments/samples-with-gobgp
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX} ghcr.io/purelb/k8gobgp=${GOBGP_IMAGE}:${GOBGP_IMAGE_TAG}
	$(KUSTOMIZE) build . > ../${PROJECT}-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: install-manifest
install-manifest: CACHE != mktemp
install-manifest: crd  ## Generate standalone install.yaml manifest (with k8gobgp)
	cd deployments/with-gobgp
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX} ghcr.io/purelb/k8gobgp=${GOBGP_IMAGE}:${GOBGP_IMAGE_TAG}
	$(KUSTOMIZE) build . > ../install-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: manifest-nobgp
manifest-nobgp: CACHE != mktemp
manifest-nobgp:  ## Generate deployment manifest without k8gobgp (with samples)
	cd deployments/samples
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX}
	$(KUSTOMIZE) build . > ../${PROJECT}-nobgp-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: install-manifest-nobgp
install-manifest-nobgp: CACHE != mktemp
install-manifest-nobgp: crd  ## Generate standalone install.yaml without k8gobgp
	cd deployments/default
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX}
	$(KUSTOMIZE) build . > ../install-nobgp-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.PHONY: install-crds
install-crds: crd  ## Generate CRDs-only install manifest (with k8gobgp CRDs)
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone deployments/crds-all > deployments/install-crds-${MANIFEST_SUFFIX}.yaml

.PHONY: install-crds-nobgp
install-crds-nobgp: crd  ## Generate CRDs-only install manifest (PureLB CRDs only)
	$(KUSTOMIZE) build deployments/crds > deployments/install-crds-nobgp-${MANIFEST_SUFFIX}.yaml

.PHONY: fetch-gobgp-crd
fetch-gobgp-crd:  ## Fetch BGPConfiguration CRD from k8gobgp ${GOBGP_TAG} release
	curl -fsSL https://github.com/purelb/k8gobgp/releases/download/${GOBGP_TAG}/install.yaml \
	  | $(KUSTOMIZE) cfg grep "kind=CustomResourceDefinition" \
	  > deployments/components/gobgp/gobgp-bgpconfig-crd.yaml

.PHONY: helm
helm:  ## Package PureLB using Helm
	rm -rf build/build
	mkdir -p build/build
	cp -r build/helm/purelb build/build/
	cp deployments/crds/purelb.io_*.yaml build/build/purelb/crds
	cp deployments/components/gobgp/gobgp-bgpconfig-crd.yaml build/build/purelb/crds/bgp.purelb.io_bgpconfigurations.yaml
	cp deployments/components/gobgp/gobgp-bgpnodestatus-crd.yaml build/build/purelb/crds/bgp.purelb.io_bgpnodestatuses.yaml
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
