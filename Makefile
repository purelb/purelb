PREFIX = purelb
SUFFIX = dev
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
all: check $(shell echo ${COMMANDS} | sed s,cmd/,image-,g)  ## Build all docker images

.PHONY: check
check:	## Run "short" tests
	go vet ./...
	NETBOX_BASE_URL=${NETBOX_BASE_URL} NETBOX_USER_TOKEN=${NETBOX_USER_TOKEN} go test -race -short ./...

.PHONY: image-%
image-%: CMD=$(subst image-,,$@)
image-%: TAG=${PREFIX}/${CMD}:${SUFFIX}
image-%:
	docker build -t ${TAG} \
	--build-arg cmd=${CMD} \
	--build-arg commit=`git describe --dirty --always` \
	--build-arg branch=`git rev-parse --abbrev-ref HEAD` \
	-f build/package/Dockerfile.${CMD} .

.PHONY: install
install: all $(shell echo ${COMMANDS} | sed s,cmd/,install-,g) ## Push images to registry

.PHONY: install-%
install-%: TAG=${PREFIX}/$(subst install-,,$@):${SUFFIX}
install-%:
	docker push ${TAG}

.PHONY: run-%
run-%:  ## Run PureLB command locally (e.g., 'make run-node-local')
	go run ./cmd/$(subst run-,,$@)

.PHONY: clean-gen
clean-gen:  ## Delete generated files
	rm -fr pkg/generated/ pkg/apis/v1/zz_generated.deepcopy.go

.PHONY: generate
generate:  ## Generate client-side stubs for our custom resources
	hack/update-codegen.sh
