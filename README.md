# PureLB - is a Service Load Balancer for Kubernetes

[PureLB](https://purelb.io) is a load-balancer orchestrator for  [Kubernetes](https://kubernetes.io) clusters. It uses standard
Linux networking and routing protocols,  and works with the operating
system to announce service addresses.

## Documentation

https://purelb.io/

## Quick Start

Install PureLB using Helm:

```sh
helm repo add purelb https://purelb.github.io/purelb/charts
helm install --create-namespace --namespace=purelb-system purelb purelb/purelb
```

Or using OCI registry (Helm 3.8+):

```sh
helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.14.0
```

For detailed installation and configuration, see https://purelb.github.io/purelb/install/

### Testing Your Installation

Deploy a simple "echo" web application:

```shell
kubectl create deployment echoserver --image=k8s.gcr.io/echoserver:1.10
```

...and then expose the deployment using a LoadBalancer service:

```shell
kubectl expose deployment echoserver --name=echoserver-service --port=80 --target-port=8080 --type=LoadBalancer
```

The PureLB allocator will allocate one or more addresses and assign them to the
service. The PureLB node agents then configure the underlying
operating system to advertise the addresses.

## Building

Run `make help` for Makefile documentation.

### CI/CD

This project uses GitHub Actions for CI/CD:
- **Tests** run on all branches and pull requests
- **Container images** are built and pushed to `ghcr.io/purelb/purelb` on main branch and tags
- **Releases** are created automatically when a version tag (e.g., `v0.14.0`) is pushed

### Local Development

#### Option 1: Build and push to registry

If you have push access to a container registry:

```sh
export KO_DOCKER_REPO=ghcr.io/purelb/purelb
export TAG=dev
ko build --base-import-paths --tags=$TAG ./cmd/allocator
ko build --base-import-paths --tags=$TAG ./cmd/lbnodeagent
```

#### Option 2: Build locally without registry access

Build images to tarballs and load directly into your cluster's container runtime:

```sh
# Build images to tarballs (requires TAG for ldflags in .ko.yaml)
export KO_DOCKER_REPO=purelb TAG=test-local
ko build --base-import-paths --tags=test-local --push=false --tarball=/tmp/allocator.tar --platform=linux/amd64 ./cmd/allocator
ko build --base-import-paths --tags=test-local --push=false --tarball=/tmp/lbnodeagent.tar --platform=linux/amd64 ./cmd/lbnodeagent

# Copy to your node(s) and import into containerd
scp /tmp/allocator.tar /tmp/lbnodeagent.tar your-node:/tmp/
ssh your-node "sudo ctr -n k8s.io images import /tmp/allocator.tar /tmp/lbnodeagent.tar"

# Deploy using kustomize (uses imagePullPolicy: Never)
kubectl apply -k deployments/default/
```

For multi-node clusters, repeat the image import on each node, or use a local registry.

## Code

* [Commands](cmd) - if you're a "top-down" learner then start here
* [Internal Code](internal) - if you're a "bottom-up" learner then start here
* [Docker Packaging](build/package)
* [Helm Packaging](build/helm)
* [Sample Configurations](configs)
* [K8s Deployment Files](deployments)

## Credits

PureLB wouldn't have been possible without MetalLB so we owe a huge
debt of gratitude to [Dave Anderson](https://www.dave.tf/) for almost
single-handedly making MetalLB happen. Thank you Dave!
