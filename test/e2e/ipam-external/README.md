# External (Sidecar) IPAM E2E

End-to-end test for PureLB's external IPAM integration: a sidecar process in
the allocator pod hands out addresses over the gRPC IPAM contract
([`api/ipam/v1/ipam.proto`](../../../api/ipam/v1/ipam.proto)), PureLB programs
them onto Services and announces them, and `kubectl get servicegroups`
reflects the sidecar's `Stats`.

## What it verifies

- An `external` ServiceGroup is parsed and a `SidecarPool` is created.
- A LoadBalancer Service gets an address allocated **by the sidecar**
  (the IP falls in the sidecar's configured pool).
- `pool-type` annotation matches the configured `announce` mode.
- For `announce: local`, the VIP lands on a node interface and is reachable.
- `.status.ipam`, `.status.allocatedIPv4`, `.status.availableIPv4` are
  populated from the sidecar's `Stats` RPC.
- `purelb_allocator_sidecar_rpc_total` records `Allocate`/`Stats`/`Release`
  with `code="OK"` and **no** failed RPCs.
- On delete, the address is withdrawn and `Release` is called on the sidecar.

## Running it (bundled sample sidecar)

By default the suite deploys the bundled sample sidecar
([`cmd/test-sidecar`](../../../cmd/test-sidecar)) into the allocator pod,
runs the tests, then removes it.

```bash
# Build + push the sample sidecar image (or use a published tag):
KO_DOCKER_REPO=ghcr.io/purelb/purelb TAG=dev \
  go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=dev ./cmd/test-sidecar

cd test/e2e/ipam-external
SIDECAR_IMAGE=ghcr.io/purelb/purelb/test-sidecar:dev ./test-ipam-external.sh --context my-cluster
```

If the sidecar image is in a private registry, pass an existing pull secret:

```bash
SIDECAR_IMAGE=ghcr.io/you/test-sidecar:dev SIDECAR_PULL_SECRET=ghcr-pull \
  ./test-ipam-external.sh --context my-cluster
```

## Using it for your own IPAM

This suite is meant to be the acceptance test when you build a sidecar for a
real IPAM (Netbox, Infoblox, an in-house system, ...). Two ways:

**A. Point the suite at your sidecar image.** It deploys your sidecar the
same way it deploys the sample, then runs the same assertions:

```bash
SIDECAR_IMAGE=ghcr.io/you/my-ipam-sidecar:v1 \
SIDECAR_PROVIDER=my-ipam \
SIDECAR_POOL_CIDR=10.20.30.0/28 \
ANNOUNCE=remote \
  ./test-ipam-external.sh --context my-cluster
```

Your sidecar must read `SIDECAR_SOCKET` (and whatever else it needs) from its
environment. If it takes different env vars, deploy it yourself (next option).

**B. Deploy your sidecar yourself, then run with `--no-deploy-sidecar`.** Add
your sidecar container to the allocator Deployment with a shared `emptyDir`
socket volume (see
[the docs](../../../website/content/docs/configuration/external-ipam/_index.md)),
then:

```bash
SIDECAR_PROVIDER=my-ipam SIDECAR_POOL_CIDR=10.20.30.0/28 \
  ./test-ipam-external.sh --no-deploy-sidecar --context my-cluster
```

## Options

| Env / flag | Default | Meaning |
|---|---|---|
| `SIDECAR_IMAGE` | `ghcr.io/purelb/purelb/test-sidecar:latest` | sidecar container image |
| `SIDECAR_PROVIDER` | `sample-ipam` | provider name (shown in `.status.ipam`) |
| `SIDECAR_SOCKET` | `/var/run/purelb/ipam.sock` | Unix socket path |
| `SIDECAR_POOL_CIDR` | derived `<node-subnet>.224/28` | CIDR the sidecar allocates from |
| `SIDECAR_PULL_SECRET` | _(none)_ | imagePullSecret for a private sidecar image |
| `ANNOUNCE` | `local` | `local` or `remote` |
| `--no-deploy-sidecar` | _(off)_ | use a sidecar already in the allocator pod |
| `--keep-sidecar` | _(off)_ | leave the sidecar deployed after the test |
| `--context NAME` | current | kubernetes context |

## Prerequisites

- PureLB v0.17.0+ installed (CRD with `spec.external` + `servicegroups/status` RBAC).
- For `announce: local`: SSH access to nodes (the suite's standard
  connectivity methodology — see the [top-level README](../README.md)).
- A `SIDECAR_POOL_CIDR` whose addresses are announceable: for `local`, within
  a node subnet; for `remote`, routable by your BGP/router setup.

## Notes

- The sample sidecar keeps state **in memory** and loses it on restart. That
  is fine for this test (it exercises the idempotent-`Allocate` contract) but
  is exactly what a production sidecar must not do — see the
  [sidecar contract](../../../website/content/docs/configuration/external-ipam/_index.md).
- After an allocator restart, an external pool's `.status` counters stay at
  their last value until the next allocation/release event
  (`SidecarPool.Contains` returns false, so `populateFromExisting` doesn't
  re-map existing external allocations). The data plane is unaffected.
