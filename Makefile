PROJECT ?= purelb
REPO ?= registry.gitlab.com/${PROJECT}
PREFIX ?= ${PROJECT}
REGISTRY_IMAGE ?= ${REPO}/${PREFIX}
SUFFIX = v0.0.0-dev
MANIFEST_SUFFIX = ${SUFFIX}
CONFIG_BASE ?= default
COMMANDS = $(shell find cmd -maxdepth 1 -mindepth 1 -type d)
NETBOX_USER_TOKEN = no-op
EPIC_WS_USERNAME = no-op
EPIC_WS_PASSWORD = no-op
NETBOX_BASE_URL = http://192.168.1.40:30080/
DEPLOYMENT_ROOT ?= deployments/${CONFIG_BASE}

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
all: check $(shell echo ${COMMANDS} | sed s,cmd/,image-,g) ## Build it all!

.PHONY: check
check:	## Run "short" tests
	go vet ./...
	EPIC_WS_USERNAME=${EPIC_WS_USERNAME} EPIC_WS_PASSWORD=${EPIC_WS_PASSWORD} NETBOX_BASE_URL=${NETBOX_BASE_URL} NETBOX_USER_TOKEN=${NETBOX_USER_TOKEN} go test -race -short ./...

.PHONY: image-%
image-%: CMD=$(subst image-,,$@)
image-%: TAG=${REGISTRY_IMAGE}/${CMD}:${SUFFIX}
image-%:
	docker build -t ${TAG} \
	--build-arg GITLAB_TOKEN \
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

.ONESHELL:
.PHONY: manifest
manifest: CACHE != mktemp
manifest:  ## Generate deployment manifest
	cd ${DEPLOYMENT_ROOT}
# cache kustomization.yaml because "kustomize edit" modifies it
	cp kustomization.yaml ${CACHE}
	kustomize edit set image registry.gitlab.com/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} registry.gitlab.com/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX}
	kustomize build . > ../${PROJECT}-${CONFIG_BASE}-${MANIFEST_SUFFIX}.yaml
# restore kustomization.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: docker-manifest
docker-manifest: ALLOCATOR_IMG=${REGISTRY_IMAGE}/allocator
docker-manifest: LBNODEAGENT_IMG=${REGISTRY_IMAGE}/lbnodeagent
docker-manifest:  ## Generate and push Docker multiarch manifest
	docker manifest create ${ALLOCATOR_IMG}:${MANIFEST_SUFFIX} ${ALLOCATOR_IMG}:amd64-${SUFFIX}
	docker manifest push ${ALLOCATOR_IMG}:${MANIFEST_SUFFIX}
	docker manifest create ${LBNODEAGENT_IMG}:${MANIFEST_SUFFIX} ${LBNODEAGENT_IMG}:amd64-${SUFFIX}
	docker manifest push ${LBNODEAGENT_IMG}:${MANIFEST_SUFFIX}

.PHONY: helm
helm:  ## Package PureLB using Helm
	helm package --version ${SUFFIX} build/helm/purelb
