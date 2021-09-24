PROJECT ?= purelb
REPO ?= registry.gitlab.com/${PROJECT}
PREFIX ?= ${PROJECT}
REGISTRY_IMAGE ?= ${REPO}/${PREFIX}
SUFFIX = v0.0.0-dev
MANIFEST_SUFFIX = ${SUFFIX}
COMMANDS = $(shell find cmd -maxdepth 1 -mindepth 1 -type d)
NETBOX_USER_TOKEN = no-op
NETBOX_BASE_URL = http://192.168.1.40:30080/

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
all: check crd $(shell echo ${COMMANDS} | sed s,cmd/,image-,g) ## Build it all!

.PHONY: check
check:	## Run "short" tests
	go vet ./...
	NETBOX_BASE_URL=${NETBOX_BASE_URL} NETBOX_USER_TOKEN=${NETBOX_USER_TOKEN} go test -race -short ./...

.PHONY: image-%
image-%: CMD=$(subst image-,,$@)
image-%: TAG=${REGISTRY_IMAGE}/${CMD}:${SUFFIX}
image-%:
	docker build -t ${TAG} \
	--build-arg cmd=${CMD} \
	--build-arg commit=`git describe --dirty --always` \
	--build-arg branch=`git rev-parse --abbrev-ref HEAD` \
	-f build/package/Dockerfile.${CMD} .

.PHONY: install
install: all $(shell echo ${COMMANDS} | sed s,cmd/,install-,g) ## Push images to registry

.PHONY: install-%
install-%: TAG=${REGISTRY_IMAGE}/$(subst install-,,$@):${SUFFIX}
install-%:
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
	controller-gen crd paths="./pkg/apis/..." output:crd:artifacts:config=deployments/crds
	cp deployments/crds/purelb.io_*.yaml build/helm/purelb/crds

.ONESHELL:
.PHONY: manifest
manifest: CACHE != mktemp
manifest:  ## Generate deployment manifest
	cd deployments/samples
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	kustomize edit set image registry.gitlab.com/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} registry.gitlab.com/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX}
	kustomize build . > ../${PROJECT}-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: docker-manifest
docker-manifest: ALLOCATOR_IMG=${REGISTRY_IMAGE}/allocator
docker-manifest: LBNODEAGENT_IMG=${REGISTRY_IMAGE}/lbnodeagent
docker-manifest:  ## Generate and push Docker multiarch manifest
	docker manifest create ${ALLOCATOR_IMG}:${MANIFEST_SUFFIX} ${ALLOCATOR_IMG}:amd64-${SUFFIX} ${ALLOCATOR_IMG}:arm64-${SUFFIX}
	docker manifest push ${ALLOCATOR_IMG}:${MANIFEST_SUFFIX}
	docker manifest create ${LBNODEAGENT_IMG}:${MANIFEST_SUFFIX} ${LBNODEAGENT_IMG}:amd64-${SUFFIX} ${LBNODEAGENT_IMG}:arm64-${SUFFIX}
	docker manifest push ${LBNODEAGENT_IMG}:${MANIFEST_SUFFIX}

.PHONY: helm
helm:  ## Package PureLB using Helm
	rm -rf build/build
	mkdir -p build/build
	cp -r build/helm/purelb build/build/

	sed \
	--expression="s~DEFAULT_REPO~${REGISTRY_IMAGE}~" \
	--expression="s~DEFAULT_TAG~${SUFFIX}~" \
	build/helm/purelb/values.yaml > build/build/purelb/values.yaml

	helm package \
	--version "${SUFFIX}" --app-version "${SUFFIX}" \
	build/build/purelb
