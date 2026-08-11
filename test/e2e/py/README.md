# PureLB e2e (pytest)

    python3 -m venv .venv && .venv/bin/pip install -e .
    .venv/bin/pytest --context prox-purelb2

Reset the cluster first, so a run starts from a known baseline:

    ../../../scripts/reset-test-cluster.sh --context prox-purelb2 --yes

## Options

| Flag | Meaning |
|------|---------|
| `--context` | kubectl context. **Required**, no default. |
| `--purelb-namespace` | install namespace (default `purelb-system`) |
| `--router-host` | upstream router, enables on-the-wire GARP/NA assertions |
| `--require a,b` | fail rather than skip when a capability is missing |

## External (sidecar) IPAM — and using it for your own IPAM

`tests/test_ipam_external.py` is also the **acceptance test** for a sidecar
built against [`api/ipam/v1/ipam.proto`](../../../api/ipam/v1/ipam.proto)
(Netbox, Infoblox, an in-house system, ...). It verifies that an `external`
ServiceGroup produces a `SidecarPool`; that a Service is allocated an
address **by the sidecar**, from the sidecar's pool, for IPv4, IPv6 and
dual-stack; that `pool-type` matches the `announce` mode; that a `local`
VIP lands on a node interface and serves traffic; that `.status` is
populated from the `Stats` RPC; that `Allocate`/`Stats`/`Release` are
recorded with `code="OK"` and none fail; and that a delete withdraws the
address and calls `Release`.

Build and push the sample sidecar with `make image-test-sidecar`
(`REPO=ghcr.io/purelb PREFIX=purelb SUFFIX=latest` for the published tag).

**A. Point it at your sidecar image.** It is deployed exactly as the sample
is, and the same assertions run:

    SIDECAR_IMAGE=ghcr.io/you/my-ipam-sidecar:v1 \
    SIDECAR_PROVIDER=my-ipam \
    SIDECAR_POOL_CIDR=10.20.30.0/28 \
    ANNOUNCE=remote \
      .venv/bin/pytest tests/test_ipam_external.py --context my-cluster

Your sidecar must read `SIDECAR_SOCKET` from its environment. If it takes
different env vars, deploy it yourself and use option B.

**B. Deploy your sidecar yourself**, adding it to the allocator Deployment
with a shared `emptyDir` socket volume (see
[the docs](../../../website/content/docs/configuration/external-ipam/_index.md)),
then:

    SIDECAR_DEPLOY=false SIDECAR_PROVIDER=my-ipam \
    SIDECAR_POOL_CIDR=10.20.30.0/28 \
      .venv/bin/pytest tests/test_ipam_external.py --context my-cluster

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
| `ANNOUNCE` | `local` | `local` or `remote` |

Requires PureLB v0.17.0+ (CRD with `spec.external` and `servicegroups/status`
RBAC), and for `announce: local` a `SIDECAR_POOL_CIDR` inside a node subnet
so the VIP is announceable.

Two things to know about the sample sidecar: it keeps state **in memory**
and loses it on restart, which is fine here because it exercises the
idempotent-`Allocate` contract but is exactly what a production sidecar
must not do. And after an allocator restart an external pool's `.status`
counters stay at their last value until the next allocate/release, because
`SidecarPool.Contains` returns false so `populateFromExisting` does not
re-map existing external allocations. The data plane is unaffected.

## Why the harness looks like this

Each of these is a bug the bash suite actually had, designed out rather
than fixed:

- `metrics.scrape` **raises** instead of returning empty. Callers used to
  write `if [ -n "$METRICS" ]`, so a failed scrape skipped the assertions
  and the test passed.
- Counter helpers take a **baseline**, so the natural thing to write is a
  delta. `> 0` on a monotonic counter asks whether something has ever
  happened, not whether it just did.
- `pod_logs` **requires** a time window. Without one, assertions matched
  lines emitted by earlier tests on other nodes.
- `pod_on_node` uses a **field selector**. Matching `kubectl get pods -o wide`
  text also matched the NAME and IP columns, and could not tell
  `purelb2-1` from `purelb2-10`.
- Fixtures **stack**, so cleanup cannot be lost. Bash keeps one EXIT trap,
  and a suite that set its own silently replaced the shared one.
- Skips are **counted and reported**, and `--require` makes them fatal.
