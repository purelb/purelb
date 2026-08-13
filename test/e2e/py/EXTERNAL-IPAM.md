# External (sidecar) IPAM — `tests/test_ipam_external.py`

Only relevant if you are working on external IPAM. If you are not,
run the suite with `--ignore=tests/test_ipam_external.py`: with no
reachable sidecar image the allocator rollout does not complete and the
whole module errors.

A sidecar process in the allocator pod hands out addresses over the gRPC
IPAM contract ([api/ipam/v1/ipam.proto](../../../api/ipam/v1/ipam.proto)),
PureLB programs them onto Services and announces them, and the
ServiceGroup status reflects the sidecar's `Stats`.

## What the 17 tests assert

An `external` ServiceGroup produces a `SidecarPool`; a Service is
allocated an address **by the sidecar**, from the sidecar's pool, for
IPv4, IPv6 and dual-stack; `pool-type` matches the `announce` mode; a
`local` VIP lands on a node interface and serves traffic; `.status` is
populated from the `Stats` RPC; `Allocate`/`Stats`/`Release` are recorded
with `code="OK"` and none fail; and a delete withdraws the address and
calls `Release`.

RPC counters are asserted as **deltas** against a baseline taken after
the sidecar rollout. The tests share module-scoped fixtures and run in
file order: allocation happens once, several tests assert on it, and the
release test tears it down last.

## Running it against the sample sidecar

Build and push it — it is deliberately not part of `make image`, being
test scaffolding rather than a shipped component:

```bash
make image-test-sidecar REPO=ghcr.io/purelb PREFIX=purelb SUFFIX=latest
.venv/bin/pytest tests/test_ipam_external.py --context <ctx>
```

Two things to know about the sample: it keeps state **in memory** and
loses it on restart, which is fine here because it exercises the
idempotent-`Allocate` contract but is exactly what a production sidecar
must not do. And after an allocator restart an external pool's `.status`
counters stay at their last value until the next allocate or release,
because `SidecarPool.Contains` returns false so `populateFromExisting`
does not re-map existing external allocations. The data plane is
unaffected.

## Running it as the acceptance test for your own sidecar

This module is also the acceptance test for any sidecar built against the
proto — Netbox, Infoblox, an in-house system.

**A. Point it at your image.** It is deployed exactly as the sample is
and the same assertions run. Your sidecar must read `SIDECAR_SOCKET` from
its environment:

```bash
SIDECAR_IMAGE=ghcr.io/you/my-ipam-sidecar:v1 \
SIDECAR_PROVIDER=my-ipam \
SIDECAR_POOL_CIDR=10.20.30.0/28 \
ANNOUNCE=remote \
  .venv/bin/pytest tests/test_ipam_external.py --context <ctx>
```

**B. Deploy the sidecar yourself** if it takes different env vars —
add it to the allocator Deployment with a shared `emptyDir` socket volume
(see [the docs](../../../website/content/docs/configuration/external-ipam/_index.md)),
then:

```bash
SIDECAR_DEPLOY=false SIDECAR_PROVIDER=my-ipam \
SIDECAR_POOL_CIDR=10.20.30.0/28 \
  .venv/bin/pytest tests/test_ipam_external.py --context <ctx>
```

| Env | Default | Meaning |
|---|---|---|
| `SIDECAR_IMAGE` | `ghcr.io/purelb/purelb/test-sidecar:latest` | sidecar container image |
| `SIDECAR_PROVIDER` | `sample-ipam` | provider name, shown in `.status.ipam` |
| `SIDECAR_SOCKET` | `/var/run/purelb/ipam.sock` | Unix socket path |
| `SIDECAR_POOL_CIDR` | derived `<node-subnet>.224/28` | IPv4 CIDR the sidecar allocates from |
| `SIDECAR_POOL_CIDR6` | derived `<node-v6-prefix>:e::/120` | IPv6 CIDR; without it the dual-stack tests skip |
| `SIDECAR_PULL_SECRET` | _(none)_ | imagePullSecret for a private image |
| `SIDECAR_DEPLOY` | `true` | `false` uses a sidecar already in the allocator pod |
| `SIDECAR_KEEP` | `false` | `true` leaves the sidecar deployed, for debugging |
| `ANNOUNCE` | `local` | `local` or `remote`; the reachability tests skip under `remote` |

Requires PureLB v0.17.0+ (CRD with `spec.external` and
`servicegroups/status` RBAC), and for `announce: local` a
`SIDECAR_POOL_CIDR` inside a node subnet so the VIP is announceable.
