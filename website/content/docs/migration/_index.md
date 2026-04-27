---
title: "Migration"
description: "Migrate from PureLB v1 API to v2."
weight: 80
---

This section is for users upgrading from PureLB v0.15.x or earlier. If you are installing PureLB for the first time, you can skip this section entirely.

The migration covers three areas:

1. **API version change** -- CRDs moved from `purelb.io/v1` to `purelb.io/v2` with field renames and the new `local`/`remote` pool type distinction.
2. **Election system change** -- Memberlist replaced by Kubernetes Leases. No user action required beyond upgrading.
3. **BGP routing change** -- Standalone routing software replaced by the integrated k8gobgp sidecar configured via the BGPConfiguration CRD.

## API Version: v1 to v2

### ServiceGroup Changes

The most significant change is the separation of pool types. In v1, `spec.local` was used for both same-subnet and routed addresses, with PureLB inferring the type. In v2, you must explicitly choose:

- `spec.local` -- Addresses on the same subnet as your nodes (announced on the node's physical interface)
- `spec.remote` -- Addresses on a different subnet (announced on kube-lb0, requires BGP)
- `spec.netbox` -- Addresses managed by an external Netbox IPAM system

**How to decide:** If your old ServiceGroup's `pool` addresses are on the same subnet as your nodes, use `spec.local`. If they are on a different subnet (and you use routing to reach them), use `spec.remote`.

#### Field Name Changes

v1 Field | v2 Field | Notes
---------|----------|------
`v4pool` | `v4pool` | Unchanged (singular shorthand)
`v6pool` | `v6pool` | Unchanged (singular shorthand)
`v4pools` | `v4pools` | Unchanged (array)
`v6pools` | `v6pools` | Unchanged (array)

New fields in v2: `multiPool`, `balancePools`, `skipIPv6DAD` (local pools only).

#### Example: Local Pool Migration

For same-subnet addresses, change only the API version:

```yaml
# Change: purelb.io/v1 -> purelb.io/v2
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
```

#### Example: Remote Pool Migration

If your v1 ServiceGroup used addresses on a different subnet from your nodes (typically with `/32` or `/128` aggregation and routing software):

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: routed
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 172.31.0.0/24
      pool: 172.31.0.1-172.31.0.100
      aggregation: /32
```

**v2 (different-subnet addresses):**
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: routed
  namespace: purelb-system
spec:
  remote:
    v4pools:
    - subnet: 172.31.0.0/24
      pool: 172.31.0.1-172.31.0.100
      aggregation: /32
```

### LBNodeAgent Changes

v1 Field | v2 Field | Notes
---------|----------|------
`localint` | `localInterface` | Renamed
`extlbint` | `dummyInterface` | Renamed
`sendgarp` | `garpConfig.enabled` | Now a structured object with count, interval, delay, verifyBeforeSend

New fields in v2: `garpConfig` (structured), `addressConfig` (lifetime control), `interfaces` (additional subnet detection).

#### Example: LBNodeAgent Migration

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localint: default
    extlbint: kube-lb0
    sendgarp: true
```

**v2:**
```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
    dummyInterface: kube-lb0
    garpConfig:
      enabled: true
```

## Election System: Memberlist to Leases

Previous versions used [Memberlist](https://github.com/hashicorp/memberlist) (a gossip protocol on port 7934) to elect which node announces each local address. This has been replaced by Kubernetes Lease-based election.

**What changed:**
- Port 7934 (UDP/TCP) is no longer used and can be closed in firewalls
- The `memberlistSecretKey` Helm value is deprecated and ignored
- Election is now deterministic (SHA256 hash) rather than gossip-based
- Configuration is via `PURELB_LEASE_DURATION`, `PURELB_RENEW_DEADLINE`, `PURELB_RETRY_PERIOD` environment variables (set via Helm `leaseConfig`)

**User action:** None required beyond upgrading. The new election system activates automatically.

## BGP Routing: Standalone to k8gobgp Sidecar

Previous versions required users to install and configure standalone routing software (such as BIRD or FRR) on cluster nodes. PureLB now ships k8gobgp as an integrated sidecar in the lbnodeagent DaemonSet.

**What changed:**
- BGP is configured via the `BGPConfiguration` CRD (`bgp.purelb.io/v1`), not routing software config files
- k8gobgp automatically imports routes from kube-lb0 (via `netlinkImport`)
- Per-node BGP status is reported in the `BGPNodeStatus` CRD

**Migration steps:**
1. Create a `BGPConfiguration` CR with your ASN, neighbors, and address families (see [BGP Configuration]({{< relref "/docs/configuration/bgp" >}}))
2. Upgrade PureLB (the k8gobgp sidecar starts automatically with the default install)
3. Verify BGP sessions: `kubectl purelb bgp sessions`
4. Once verified, remove the standalone routing software pods/configuration

## Migration Procedure

1. **Back up existing CRs:**
   ```sh
   kubectl get servicegroups -n purelb-system -o yaml > servicegroups-backup.yaml
   kubectl get lbnodeagents -n purelb-system -o yaml > lbnodeagents-backup.yaml
   ```

2. **Convert your YAML files** using the examples above. For each ServiceGroup, decide whether it should be `spec.local` or `spec.remote`.

3. **Install the v2 CRDs** (they replace the v1 CRDs):
   ```sh
   kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-v0.16.3.yaml
   ```

4. **Apply converted CRs:**
   ```sh
   kubectl apply -f servicegroups-v2.yaml
   kubectl apply -f lbnodeagents-v2.yaml
   ```

5. **Upgrade PureLB:**
   ```sh
   kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-v0.16.3.yaml
   ```

6. **Verify:** Existing services should retain their allocated addresses. Check with:
   ```sh
   kubectl purelb status
   kubectl purelb services
   ```

## Rollback

To roll back, re-apply the v1 CRDs and your backed-up v1 CRs, then downgrade the PureLB deployment to the previous version.
