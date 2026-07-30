---
title: "External IPAM (Sidecar)"
description: "Integrate an external IPAM system with PureLB using a sidecar process."
weight: 40
---

PureLB can delegate address allocation to an external IPAM system through a
**sidecar process** running in the allocator pod. The allocator talks to the
sidecar over a Unix domain socket using a small gRPC contract; the sidecar
owns its own configuration, persistence, and the conversation with the
backing IPAM (Netbox, Infoblox, BlueCat, an in-house system, etc.).

This keeps IPAM-specific code out of PureLB: you (or a community project)
ship a sidecar that implements the contract, and PureLB integrates it
without a new PureLB release.

> [!NOTE]
> The sidecar protocol is intentionally minimal (three RPCs). PureLB holds
> **no** authoritative IPAM state for external pools — the sidecar is the
> source of truth.

## How it works

1. You deploy a sidecar container alongside the allocator, sharing an
   `emptyDir` volume for the Unix socket.
2. You create a ServiceGroup with `spec.external` pointing at the socket.
3. When a LoadBalancer Service requests an address from that pool, the
   allocator calls the sidecar's `Allocate` RPC and programs the returned
   IP(s) onto the Service.
4. On deletion, the allocator calls `Release`.
5. For `kubectl get servicegroups` display, the allocator periodically calls
   `Stats`.

## ServiceGroup

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: external-pool
  namespace: purelb-system
spec:
  external:
    # provider is a cosmetic display name surfaced in .status.ipam.
    # PureLB does NOT verify it against the sidecar's identity.
    provider: my-ipam
    # socket is the Unix domain socket the allocator dials. Defaults to
    # /var/run/purelb/ipam.sock.
    socket: /var/run/purelb/ipam.sock
    # announce selects the announcement mechanism for allocated addresses:
    #   local  - announced on the node's primary interface
    #   remote - announced on the kube-lb0 dummy interface (for BGP/routing)
    announce: remote
```

## Deploying the sidecar

The sidecar runs as a second container in the allocator Deployment, sharing
an `emptyDir` volume mounted at the socket's parent directory in **both**
containers:

```yaml
spec:
  template:
    spec:
      volumes:
      - name: ipam-socket
        emptyDir: {}
      containers:
      - name: allocator
        # ... existing allocator spec ...
        volumeMounts:
        - name: ipam-socket
          mountPath: /var/run/purelb
      - name: my-ipam-sidecar
        image: ghcr.io/example/my-ipam-sidecar:v1
        # The sidecar reads its OWN config; PureLB pushes nothing.
        env:
        - name: SIDECAR_SOCKET
          value: /var/run/purelb/ipam.sock
        # ... whatever else your sidecar needs (backend URL, credentials ref) ...
        volumeMounts:
        - name: ipam-socket
          mountPath: /var/run/purelb
```

> [!IMPORTANT]
> **Do not add a liveness/readiness probe to the allocator container that
> depends on the sidecar being reachable.** A probe that fails when the
> sidecar is down would crash-loop the allocator and take down **all** VIPs —
> including Local/Remote (cluster-IPAM) pools that have nothing to do with
> the sidecar. Sidecar problems surface through metrics and Service events,
> not by restarting the allocator.

## Sidecar implementation contract

A conforming sidecar **MUST** satisfy all of the following. These are
requirements, not suggestions — PureLB does not enforce them, and violating
them causes subtle production bugs.

1. **Idempotent `Allocate`.** Repeated `Allocate(pool, service)` for the same
   `(pool, service)` MUST return the same IP set. The allocator may retry on
   transient errors; the sidecar must not double-allocate.
2. **Idempotent `Release`.** Releasing a service that already holds no
   addresses is success, not an error.
3. **Persistent state.** The sidecar MUST persist allocation state across
   restarts. The allocator does **not** replay state to the sidecar at
   startup — if the sidecar loses state, addresses leak. (In-memory sidecars
   are demo/test tier only.)
4. **Self-configuring.** The sidecar reads its own configuration (env vars,
   mounted files, ConfigMaps, Secrets). PureLB pushes no configuration;
   `spec.external` carries only what PureLB needs to dial the socket.
5. **Stale-socket cleanup.** Call `os.Remove(socketPath)` (or equivalent)
   before binding the listener on startup, or a restart fails to bind.
6. **Multi-pool support.** One sidecar process MUST serve multiple pools.
   Several ServiceGroups may share one socket; the sidecar tracks per-pool
   state keyed on `AllocateRequest.pool` (the ServiceGroup name).
7. **Concurrent RPCs.** gRPC over HTTP/2 multiplexes; the sidecar MUST handle
   concurrent calls (e.g. `Allocate` while `Stats` is in flight).

## The gRPC contract

The protobuf schema lives at
[`api/ipam/v1/ipam.proto`](https://github.com/purelb/purelb/blob/main/api/ipam/v1/ipam.proto)
and is vendor-shareable — import it to generate stubs in your sidecar's
language.

Three RPCs:

| RPC | Purpose |
|---|---|
| `Allocate` | Return address(es) for a service. Idempotent on `(pool, service)`. |
| `Release` | Free a service's address(es). Idempotent. |
| `Stats` | Report usage/capacity for `kubectl get sg` display. |

Errors are returned via **gRPC status codes** (no error fields in the
messages). Recommended mapping:

| Code | Meaning |
|---|---|
| `OK` | success |
| `AlreadyExists` | idempotent allocate (service already had an address) |
| `ResourceExhausted` | pool full |
| `InvalidArgument` | malformed request |
| `Unavailable` | transient; the allocator surfaces a Service event and retries |

A future `GetInfo` RPC is reserved (service slot 4) for capability
negotiation; v0.17.0 does not use it.

## Sample sidecar

A minimal, in-memory reference sidecar — **`purelb-sample-ipam`** — is
published separately as documentation-by-example. It implements all three
RPCs in a couple hundred lines of Go with no external dependencies. Fork it
and replace the in-memory pool with your backend.

> The sample keeps state in process memory only and therefore **loses
> allocations on restart**. That is acceptable for a demo (it exercises the
> idempotent-`Allocate` contract on the allocator side) but is exactly what
> contract requirement #3 forbids for production.

## Observability

External-IPAM activity is exported by the allocator:

| Metric | Labels | Meaning |
|---|---|---|
| `purelb_allocator_sidecar_rpc_total` | `socket`, `method`, `code` | RPC count by gRPC status code |
| `purelb_allocator_sidecar_rpc_duration_seconds` | `socket`, `method` | RPC latency histogram |

There is no separate "connected" gauge — the RPC error rate is the
connectivity signal. Recommended alerts:

```promql
# Sidecar RPC error rate above 5% over 5 minutes
sum by (socket) (rate(purelb_allocator_sidecar_rpc_total{code!="OK"}[5m]))
  / sum by (socket) (rate(purelb_allocator_sidecar_rpc_total[5m])) > 0.05

# Sidecar p99 latency above 1s
histogram_quantile(0.99,
  rate(purelb_allocator_sidecar_rpc_duration_seconds_bucket[5m])) > 1

# ServiceGroup status writes failing (e.g. missing RBAC)
rate(purelb_allocator_sg_status_writes_total{outcome!="success"}[5m]) > 0
```

## Behavior when the sidecar is unavailable

If the sidecar is down, `Allocate` calls fail with gRPC `Unavailable`; the
allocator surfaces an event on the affected Service and the address stays
unallocated until the sidecar returns. **Already-announced VIPs are
unaffected** — the lbnodeagents keep announcing addresses that were already
assigned. gRPC reconnects transparently once the sidecar is back.

External pools are independent of Local/Remote pools: a sidecar outage never
affects cluster-IPAM allocations.
