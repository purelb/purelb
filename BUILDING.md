# Building PureLB

This document covers building PureLB from source, the CI/CD pipeline, and
local development workflows. For installation and usage, see the
[README](README.md).

## Building

All build operations are driven by `make`. Container images are built with
[ko](https://ko.build/) under the hood, pinned to a known version in the
Makefile — you don't need to install ko yourself. Run `make help` for the
full target list; the most useful are:

| Target | When to use it |
|---|---|
| `make check` | Run `go vet` and race tests (also regenerates client stubs) |
| `make generate` | Regenerate client stubs after editing `pkg/apis/` |
| `make crd` | Regenerate CRD manifests after changing API types |
| `make image` | Build allocator + lbnodeagent container images |
| `make manifest` | Build the deployment YAML bundle via kustomize |
| `make helm` | Package the Helm chart |
| `make scan` | Run `govulncheck` against all packages |
| `make plugin` | Build the `kubectl-purelb` plugin binary |

### First build in a fresh worktree

`pkg/generated/` is gitignored, so a new checkout has no client code. Run
`make generate` once before your first `make image` or direct `ko build`, or
you will see:

```
no required module provides package purelb.io/pkg/generated/clientset/versioned
```

`make check` and `make image` both depend on `generate` as a prerequisite, so
those entry points handle it for you.

## CI/CD

This project uses GitHub Actions for CI/CD:

- **Tests** run on all branches and pull requests
- **Container images** are built and pushed to `ghcr.io/purelb/purelb` on main branch and tags
- **Releases** are created automatically when a version tag (e.g., `v0.16.6`) is pushed

## Local Development

### Running locally against a cluster

For fast inner-loop iteration, run either component directly on your host
against a test cluster — no image build, no rollout. Point your kubeconfig at
the target cluster, then:

```shell
make run-allocator
PURELB_NODE_NAME=<node-name> make run-lbnodeagent
```

The node agent needs `PURELB_NODE_NAME` to know which node's election state
it owns. This is the fastest way to test allocator or announcer logic
changes without waiting on a container build.

### Building images to your own registry

Pushing to `ghcr.io/purelb/purelb` is restricted to project maintainers —
for local development, use a registry you control. Set `REGISTRY_IMAGE` to
your registry path and `SUFFIX` to a tag of your choice:

```shell
export REGISTRY_IMAGE=<your-registry>/<your-org>/purelb
export SUFFIX=dev
make image
```

Then generate a deployment manifest that references the images you just
pushed and apply it:

```shell
make install-manifest-nobgp
kubectl apply -f deployments/install-nobgp-dev.yaml
```

`make install-manifest-nobgp` substitutes `REGISTRY_IMAGE` and `SUFFIX` into
the manifest via kustomize, so the generated YAML pulls exactly the tag you
just built. Use `install-manifest` (without the `-nobgp` suffix) if you want
k8gobgp bundled in.

### Building images without a registry

If you have no registry at all, ko can write multi-arch tarballs you load
directly into each node's containerd:

```shell
export REGISTRY_IMAGE=<your-registry>/<your-org>/purelb
export SUFFIX=test-local

go run github.com/google/ko@v0.17.1 build --base-import-paths \
    --tags=$SUFFIX --push=false --tarball=/tmp/allocator.tar \
    ./cmd/allocator

go run github.com/google/ko@v0.17.1 build --base-import-paths \
    --tags=$SUFFIX --push=false --tarball=/tmp/lbnodeagent.tar \
    ./cmd/lbnodeagent

# Copy tarballs to your node(s) and import into containerd
scp /tmp/allocator.tar /tmp/lbnodeagent.tar your-node:/tmp/
ssh your-node "sudo ctr -n k8s.io images import /tmp/allocator.tar /tmp/lbnodeagent.tar"

# Generate a matching manifest and apply it
make install-manifest-nobgp
kubectl apply -f deployments/install-nobgp-test-local.yaml
```

`REGISTRY_IMAGE` still matters here even though nothing is pushed — ko uses
it to name the image inside the tarball, and `make install-manifest-nobgp`
uses the same value to write the matching image reference into the manifest.
Set them to the same thing and the imported image name will match what the
manifest asks Kubernetes to pull.

Repeat the `ctr import` on every node. The default multi-arch tarballs
(`linux/amd64` + `linux/arm64`) let the same tarball work on mixed-architecture
clusters without rebuilding.

### Building the kubectl-purelb plugin from source

The plugin is a standalone Go binary with no container build:

```shell
make plugin
sudo mv kubectl-purelb /usr/local/bin/
```

See the [README](README.md#kubectl-purelb-plugin-optional) for the
pre-built binary download instructions.

## Testing

### Unit tests

```shell
make check           # go vet + race tests (also regenerates client stubs)
make test-coverage   # coverage profile plus a per-package summary
```

### End-to-end tests

The e2e suite is pytest, in [test/e2e/py/](test/e2e/py/). It runs against a
real cluster — it allocates addresses, taints nodes, restarts agents and
captures packets, so point it at a test cluster, never a production one.

```shell
cd test/e2e/py
python3 -m venv .venv && .venv/bin/pip install -e .

# Start from a known baseline. The suite asserts on deltas, so leftover
# Services and ServiceGroups from a previous run skew them.
../../../scripts/reset-test-cluster.sh --context <ctx> --yes

.venv/bin/pytest --context <ctx> [--router-host <frr-host>]
```

Tests declare what they need with `@pytest.mark.requires(...)` — `multi-node`,
`multi-subnet`, `ipv6`, `dual-homed`, `router` — and skip when the cluster
lacks it. Skips are counted and printed in the summary, so "green" and "green
having tested everything" stay distinguishable. Use `--require ipv6,router` to
turn a skip of those capabilities into a failure, which is what CI should do.

A few need more than a cluster:

| Capability | What it needs |
|---|---|
| `router` | SSH to an FRR host with passwordless `sudo` and `tcpdump`. Without it the BGP and on-the-wire announcement tests skip. |
| `ipv6` | Nodes with global IPv6 addresses. Every module covers both families; without this half of each is skipped. |
| upgrade tests | `PURELB_TEST_UPGRADE=1`. They temporarily remove the ServiceGroup CRD's status subresource, which degrades status reporting cluster-wide while they run, so they are opt-in. |

See [test/e2e/README.md](test/e2e/README.md) and
[test/e2e/py/README.md](test/e2e/py/README.md) for the options and how the
harness is put together.
