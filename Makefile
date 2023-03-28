PROJECT ?= purelb
REPO ?= registry.gitlab.com/${PROJECT}
PREFIX ?= ${PROJECT}
REGISTRY_IMAGE ?= ${REPO}/${PREFIX}
SUFFIX = v0.0.0-dev
MANIFEST_SUFFIX = ${SUFFIX}
COMMANDS = $(shell find cmd -maxdepth 1 -mindepth 1 -type d)
NETBOX_USER_TOKEN = no-op
NETBOX_BASE_URL = http://192.168.1.40:30080/

# Tools that we use.
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.7.0
KUSTOMIZE = go run sigs.k8s.io/kustomize/kustomize/v4@v4.5.2
HELM = go run helm.sh/helm/v3/cmd/helm@v3.11

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
.PHONY: all
all: check crd image ## Build it all!

.PHONY: check
check:	## Run "short" tests
	go vet ./...
	NETBOX_BASE_URL=${NETBOX_BASE_URL} NETBOX_USER_TOKEN=${NETBOX_USER_TOKEN} go test -race -short ./...

.PHONY: image
image: TAG=${REGISTRY_IMAGE}/${PROJECT}:${SUFFIX}
image:
	docker build -t ${TAG} \
	--build-arg commit=`git describe --dirty --always` \
	--build-arg branch=`git rev-parse --abbrev-ref HEAD` \
	.

.PHONY: install
install: TAG=${REGISTRY_IMAGE}/${PROJECT}:${SUFFIX}
install:
	docker push ${TAG}

.PHONY: run-%
run-%:  ## Run PureLB command locally (e.g., 'make run-allocator')
	go run ./cmd/$(subst run-,,$@)

.PHONY: clean-gen
clean-gen:  ## Delete generated files
	rm -fr pkg/generated/
	rm -fr deployments/${PROJECT}-*.yaml

.PHONY: generate
generate:  ## Generate client-side stubs for our custom resources
	hack/update-codegen.sh

.PHONY: crd
crd: ## Generate CRDs from golang api structs
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=deployments/crds
	cp deployments/crds/purelb.io_*.yaml build/helm/purelb/crds

.ONESHELL:
.PHONY: manifest
manifest: CACHE != mktemp
manifest:  ## Generate deployment manifest
	cd deployments/samples
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image registry.gitlab.com/purelb/purelb/purelb=${REGISTRY_IMAGE}/purelb:${SUFFIX}
	$(KUSTOMIZE) build . > ../${PROJECT}-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: docker-manifest
docker-manifest: IMG=${REGISTRY_IMAGE}/${PROJECT}
docker-manifest:  ## Generate and push Docker multiarch manifest
	docker manifest create ${IMG}:${MANIFEST_SUFFIX} ${IMG}:amd64-${SUFFIX} ${IMG}:arm64-${SUFFIX}
	docker manifest push ${IMG}:${MANIFEST_SUFFIX}

.PHONY: helm
helm:  ## Package PureLB using Helm
	rm -rf build/build
	mkdir -p build/build
	cp -r build/helm/purelb build/build/
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
