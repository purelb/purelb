# Migrating PureLB from v1 to v2 API

This guide covers migrating PureLB custom resources from the v1 API to the v2 API.

## Overview of Changes

### ServiceGroup Changes

The v2 API introduces clearer separation between **local** and **remote** pool types:

| v1 | v2 | Description |
|----|----|----|
| `spec.local` | `spec.local` | Addresses on the **same subnet** as nodes (announced via ARP/NDP) |
| `spec.local` | `spec.remote` | Addresses on a **different subnet** (announced via dummy interface for BGP) |
| `spec.netbox` | `spec.netbox` | External IPAM (unchanged) |

**Key differences:**
- v2 requires exactly one of `local`, `remote`, or `netbox` (enforced by validation)
- Field naming changed from snake_case to camelCase: `v4pool` → `v4Pool`
- Legacy single-stack fields (`subnet`, `pool`, `aggregation`) removed - use `v4Pool`/`v6Pool`
- New option: `skipIPv6DAD` to disable Duplicate Address Detection

### LBNodeAgent Changes

| v1 Field | v2 Field | Notes |
|----------|----------|-------|
| `spec.local.localint` | `spec.local.localInterface` | Renamed |
| `spec.local.extlbint` | `spec.local.dummyInterface` | Renamed |
| `spec.local.sendgarp` | Removed | Use `garpConfig.enabled` instead |
| `spec.local.garpConfig` | `spec.local.garpConfig` | Unchanged |
| `spec.local.addressConfig` | `spec.local.addressConfig` | Unchanged |
| - | `spec.local.subnets` | New: additional interfaces for subnet detection |

## Deciding Between Local and Remote

The most important decision during migration is whether each ServiceGroup should use `local` or `remote`:

### Use `local` when:
- Your LoadBalancer IP addresses are on the **same subnet** as your Kubernetes nodes
- Traffic reaches services via Layer 2 (ARP/NDP) without routing
- Example: Nodes are on 192.168.1.0/24 and your pool is 192.168.1.200-192.168.1.250

### Use `remote` when:
- Your LoadBalancer IP addresses are on a **different subnet** from your nodes
- You use a routing protocol (BGP via BIRD, etc.) to announce addresses
- Example: Nodes are on 10.0.0.0/24 but your pool is 203.0.113.0/24

## Migration Steps

### 1. Backup Existing Resources

```bash
kubectl get servicegroups.purelb.io -A -o yaml > backup-servicegroups.yaml
kubectl get lbnodeagents.purelb.io -A -o yaml > backup-lbnodeagents.yaml
```

### 2. Run the Migration Script

```bash
# Dry run first to see what would be migrated
./scripts/migrate-v1-to-v2.sh --dry-run

# Run the actual migration
./scripts/migrate-v1-to-v2.sh --output-dir ./migrated
```

The script will:
- Export all v1 ServiceGroups and LBNodeAgents
- Convert them to v2 format
- Write the converted YAML to the output directory

### 3. Review and Edit Migrated Files

**Important:** The migration script defaults all ServiceGroups to `local` type. You must review each ServiceGroup and change to `remote` if appropriate.

Example - changing from local to remote:

```yaml
# Before (migrated default):
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: bgp-pool
spec:
  local:    # <-- This needs to change
    v4Pool:
      pool: "203.0.113.0/28"
      subnet: "203.0.113.0/24"

# After (corrected for BGP use case):
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: bgp-pool
spec:
  remote:   # <-- Changed to remote
    v4Pool:
      pool: "203.0.113.0/28"
      subnet: "203.0.113.0/24"
```

### 4. Update CRDs

Apply the new v2 CRDs. This will add the v2 API version:

```bash
kubectl apply -f deployments/crds/
```

### 5. Apply Migrated Resources

```bash
kubectl apply -f ./migrated/
```

### 6. Verify Migration

```bash
# Check ServiceGroups
kubectl get servicegroups.purelb.io -A

# Check LBNodeAgents
kubectl get lbnodeagents.purelb.io -A

# Verify services are still working
kubectl get svc -A | grep LoadBalancer
```

## Example Migrations

### Single-Stack IPv4 Local Pool

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: default
spec:
  local:
    subnet: "192.168.1.0/24"
    pool: "192.168.1.200-192.168.1.210"
    aggregation: default
```

**v2:**
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
spec:
  local:
    v4Pool:
      pool: "192.168.1.200-192.168.1.210"
      subnet: "192.168.1.0/24"
      aggregation: default
```

### Dual-Stack Local Pool

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: dual-stack
spec:
  local:
    v4pool:
      pool: "192.168.1.200-192.168.1.210"
      subnet: "192.168.1.0/24"
      aggregation: default
    v6pool:
      pool: "fd53:9ef0:8683::1-fd53:9ef0:8683::10"
      subnet: "fd53:9ef0:8683::/120"
      aggregation: default
```

**v2:**
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: dual-stack
spec:
  local:
    v4Pool:
      pool: "192.168.1.200-192.168.1.210"
      subnet: "192.168.1.0/24"
      aggregation: default
    v6Pool:
      pool: "fd53:9ef0:8683::1-fd53:9ef0:8683::10"
      subnet: "fd53:9ef0:8683::/120"
      aggregation: default
```

### BGP/Remote Pool

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: bgp-pool
spec:
  local:
    v4pool:
      pool: "203.0.113.0/28"
      subnet: "203.0.113.0/24"
      aggregation: /32
```

**v2:**
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: bgp-pool
spec:
  remote:  # Note: "remote" instead of "local"
    v4Pool:
      pool: "203.0.113.0/28"
      subnet: "203.0.113.0/24"
      aggregation: /32
```

### LBNodeAgent with GARP

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  name: default
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
spec:
  local:
    localInterface: default
    dummyInterface: kube-lb0
    garpConfig:
      enabled: true
      initialDelay: "100ms"
      count: 3
      interval: "500ms"
      verifyBeforeSend: true
```

### LBNodeAgent with Custom GARP Settings

**v1:**
```yaml
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  name: default
spec:
  local:
    localint: default
    extlbint: kube-lb0
    garpConfig:
      enabled: true
      initialDelay: "200ms"
      count: 5
      interval: "1s"
```

**v2:**
```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
spec:
  local:
    localInterface: default
    dummyInterface: kube-lb0
    garpConfig:
      enabled: true
      initialDelay: "200ms"
      count: 5
      interval: "1s"
      verifyBeforeSend: true
```

## Troubleshooting

### "exactly one of local, remote, or netbox must be specified"

This validation error occurs if a ServiceGroup has:
- No pool type specified
- Multiple pool types specified

Fix by ensuring exactly one of `local`, `remote`, or `netbox` is present.

### Services not getting IPs after migration

1. Check the allocator logs: `kubectl logs -n purelb deployment/allocator`
2. Verify the ServiceGroup was migrated correctly: `kubectl get sg <name> -o yaml`
3. Ensure the `purelb.io/service-group` annotation on services matches the ServiceGroup name

### Addresses not being announced

1. Check the lbnodeagent logs: `kubectl logs -n purelb daemonset/lbnodeagent`
2. Verify the LBNodeAgent config: `kubectl get lbna -o yaml`
3. Check that lease-based election is working: `kubectl get leases -n purelb`

## Rollback

If you need to rollback:

1. Re-apply the v1 CRDs
2. Apply your backup files:
   ```bash
   kubectl apply -f backup-servicegroups.yaml
   kubectl apply -f backup-lbnodeagents.yaml
   ```

Note: v1 resources should continue to work as the v1 API is still supported during the transition period.
