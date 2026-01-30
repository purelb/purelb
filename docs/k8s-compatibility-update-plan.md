# PureLB Kubernetes Compatibility Update Plan

## Overview

This plan addresses 6 Kubernetes compatibility issues to prepare PureLB for modern Kubernetes clusters before making major operational changes.

**Target:** Kubernetes 1.25+ (clean migration, no backward compatibility with older versions)

**Approach:**
- Clean EndpointSlice migration (no dual-mode Endpoints watching)
- Full typed workqueue migration with union type for queue items

---

## ⚠️ Senior Engineering Review Findings

### Critical Issues Identified:

1. **Semantic Bug in Proposed `nodeHasHealthyEndpoint`** - The proposed implementation does NOT preserve original semantics. Original code tracks per-IP readiness across ALL ports; proposed returns on first ready endpoint.

2. **EndpointSlice Aggregation Missing** - Must add custom indexer to fetch all slices per service (multiple slices per service).

3. **Security Gap** - PSP removal without PSA namespace labels leaves enforcement gap. Must add seccomp profiles for `restricted` compliance.

4. **Operational Risk** - Recommend splitting into TWO releases to reduce blast radius.

---

## 🔴 RED TEAM FINDINGS (World-Class K8s Experts)

### API Expert (Tim Hockin - EndpointSlice Designer):

| Issue | Severity | Description |
|-------|----------|-------------|
| **Wrong Label Selector** | CRITICAL | `LabelManagedBy=endpointslice-controller.k8s.io` ONLY matches built-in controller. Misses: Istio, Consul, manually-created slices, external service discovery. |
| **Event Handler Bug** | CRITICAL | `MetaNamespaceKeyFunc` on EndpointSlice returns `namespace/slice-name` not `namespace/service-name`. Will queue wrong keys. |
| **Missing Serving/Terminating** | HIGH | Only checks `Ready` condition. Should also check `Serving` for graceful termination. |
| **Race Conditions** | HIGH | Multi-slice updates not atomic. Can see partial state during deployments. |
| **Dual-Stack AddressType** | MEDIUM | No filtering by `AddressType`. IPv4/IPv6 slices conflated. |
| **nil NodeName** | MEDIUM | External endpoints have nil NodeName - silently ignored. |

### Security Expert (Liz Rice - Container Security Author):

| Issue | Severity | Description |
|-------|----------|-------------|
| **Namespace PSA Regression** | CRITICAL | `privileged` PSA for entire namespace allows ANY pod to be fully privileged. Old PSP was per-workload. |
| **lbnodeagent RBAC Excessive** | CRITICAL | Has `update` on ALL Services cluster-wide. Should be read-only. |
| **Netbox Token Exposure** | HIGH | lbnodeagent mounts Netbox secret but doesn't need it. Token theft risk. |
| **NET_ADMIN+hostNetwork** | HIGH | Can ARP spoof, route inject, DNS hijack entire cluster. |
| **No Seccomp for lbnodeagent** | MEDIUM | Only allocator gets seccomp. lbnodeagent has full syscall surface. |

*Note: Memberlist key issue deferred - memberlist being removed in subsequent update.*

### Reliability Expert (Charity Majors / Kelsey Hightower):

| Issue | Severity | Description |
|-------|----------|-------------|
| **Mixed Version Chaos** | CRITICAL | During rolling update: old agents see Endpoints, new see EndpointSlice. Different "healthy" views = traffic blackhole. |
| **Orphaned IP State** | HIGH | `svcIngresses` map is in-memory. Crash loses announced IPs. No reconciliation on startup. |
| **No Canary Path** | HIGH | DaemonSet is all-or-nothing. Cannot run mixed versions safely. |
| **Insufficient Observability** | HIGH | No metrics for: election state, cache freshness, duplicate detection. |

*Note: Memberlist issues (Leave timeout, thundering herd) deferred - memberlist being removed in subsequent update.*

---

## Phase 1: Critical Changes

### 1.1 Endpoints → EndpointSlice Migration

**Why:** Endpoints API is deprecated in Kubernetes 1.33+ and generates API warnings. EndpointSlice has been GA since K8s 1.21.

**Scope:** Only the LBNodeAgent uses Endpoints data. The Allocator ignores it completely.

**Key insight:** Only ONE function actually reads Endpoint data: `nodeHasHealthyEndpoint()` in [announcer_local.go:398-424](internal/local/announcer_local.go#L398-L424). It checks if a node has healthy endpoints when `ExternalTrafficPolicy=Local`.

#### Files to Modify:

| File | Change |
|------|--------|
| [internal/k8s/k8s.go](internal/k8s/k8s.go) | Change Endpoints watcher to EndpointSlice watcher (lines 182-207) |
| [internal/k8s/k8s.go](internal/k8s/k8s.go) | Update `ServiceChanged` callback signature (line 63, 100) |
| [internal/k8s/k8s.go](internal/k8s/k8s.go) | Update `sync()` to fetch EndpointSlices by label `kubernetes.io/service-name` (line 386-396) |
| [internal/lbnodeagent/announcer.go](internal/lbnodeagent/announcer.go) | Change `SetBalancer` signature: `(*v1.Service, *v1.Endpoints)` → `(*v1.Service, []*discoveryv1.EndpointSlice)` (line 31) |
| [internal/local/announcer_local.go](internal/local/announcer_local.go) | Update `SetBalancer` and rewrite `nodeHasHealthyEndpoint()` for EndpointSlice (lines 135, 398-424) |
| [cmd/lbnodeagent/controller.go](cmd/lbnodeagent/controller.go) | Update `ServiceChanged` signature |
| [internal/allocator/controller.go](internal/allocator/controller.go) | Update interface signature (line 32) |
| [internal/allocator/service.go](internal/allocator/service.go) | Update implementation signature (line 33) |
| [build/helm/purelb/templates/clusterrole-lbnodeagent.yaml](build/helm/purelb/templates/clusterrole-lbnodeagent.yaml) | Add `discovery.k8s.io/endpointslices` permissions |

#### CORRECTED `nodeHasHealthyEndpoint` implementation:

**⚠️ Critical:** Must preserve original semantics - an endpoint is healthy only if ALL ports are ready.

```go
func nodeHasHealthyEndpoint(slices []*discoveryv1.EndpointSlice, node string) bool {
    // Track per-IP readiness across all slices (slices are per-port)
    // Same IP may appear in multiple slices with different ready states
    ready := map[string]bool{}

    for _, slice := range slices {
        for _, endpoint := range slice.Endpoints {
            if endpoint.NodeName == nil || *endpoint.NodeName != node {
                continue
            }
            isReady := endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready
            for _, addr := range endpoint.Addresses {
                if existing, ok := ready[addr]; ok {
                    // If ANY port is not ready, mark endpoint as not ready
                    ready[addr] = existing && isReady
                } else {
                    ready[addr] = isReady
                }
            }
        }
    }

    for _, r := range ready {
        if r {
            return true // At least one fully healthy endpoint
        }
    }
    return false
}
```

#### EndpointSlice Aggregation (custom indexer required):

```go
// Add to internal/k8s/k8s.go
const serviceNameIndex = "serviceName"

func serviceNameIndexFunc(obj interface{}) ([]string, error) {
    slice := obj.(*discoveryv1.EndpointSlice)
    serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
    if !ok {
        return nil, nil
    }
    return []string{slice.Namespace + "/" + serviceName}, nil
}

// Usage: fetch all slices for a service
slices, err := c.epSliceIndexer.ByIndex(serviceNameIndex, svcName)
```

#### ⚠️ RED TEAM FIX: Remove LabelManagedBy filter

```go
// DO NOT filter by LabelManagedBy - it misses Istio, Consul, manual slices
// Instead, watch ALL slices and filter by kubernetes.io/service-name label
sliceInformer := cache.NewSharedIndexInformer(
    cache.NewListWatchFromClient(
        client.DiscoveryV1().RESTClient(),
        "endpointslices",
        corev1.NamespaceAll,
        fields.Everything(),
    ),
    &discoveryv1.EndpointSlice{},
    0,
    cache.Indexers{
        cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
        serviceNameIndex:     serviceNameIndexFunc,  // Custom indexer
    },
)
```

#### ⚠️ RED TEAM FIX: Event handler must extract service name from label

```go
AddFunc: func(obj interface{}) {
    slice := obj.(*discoveryv1.EndpointSlice)
    // DO NOT use MetaNamespaceKeyFunc - it returns slice name, not service name!
    serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
    if !ok { return }
    key := slice.Namespace + "/" + serviceName
    c.queue.Add(svcKey(key))
},
```

#### ⚠️ RED TEAM FIX: Check Serving condition for graceful termination

```go
func nodeHasHealthyEndpoint(slices []*discoveryv1.EndpointSlice, node string) bool {
    ready := map[string]bool{}
    for _, slice := range slices {
        for _, endpoint := range slice.Endpoints {
            if endpoint.NodeName == nil || *endpoint.NodeName != node { continue }

            // Check Ready OR Serving (for graceful termination)
            isHealthy := (endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready) ||
                         (endpoint.Conditions.Serving != nil && *endpoint.Conditions.Serving)

            for _, addr := range endpoint.Addresses {
                if existing, ok := ready[addr]; ok {
                    ready[addr] = existing && isHealthy
                } else {
                    ready[addr] = isHealthy
                }
            }
        }
    }
    for _, r := range ready {
        if r { return true }
    }
    return false
}
```

**Risk:** HIGH - Core interface change affecting multiple files. Requires thorough testing.

---

### 1.2 PodSecurityPolicy Removal + Security Hardening

**Why:** PSP was removed in Kubernetes 1.25. Templates use `policy/v1beta1` which no longer exists.

**Current state:** PSP is disabled by default (`podSecurityPolicy.enabled: false`), but templates and RBAC references still exist.

#### Files to Modify:

| File | Change |
|------|--------|
| [build/helm/purelb/templates/podsecuritypolicy-allocator.yaml](build/helm/purelb/templates/podsecuritypolicy-allocator.yaml) | DELETE |
| [build/helm/purelb/templates/podsecuritypolicy-lbnodeagent.yaml](build/helm/purelb/templates/podsecuritypolicy-lbnodeagent.yaml) | DELETE |
| [build/helm/purelb/templates/clusterrole-allocator.yaml](build/helm/purelb/templates/clusterrole-allocator.yaml) | Remove PSP `use` rule |
| [build/helm/purelb/templates/clusterrole-lbnodeagent.yaml](build/helm/purelb/templates/clusterrole-lbnodeagent.yaml) | Remove PSP `use` rule |
| [build/helm/purelb/values.yaml](build/helm/purelb/values.yaml) | Remove `podSecurityPolicy` sections, add seccomp |
| [deployments/default/purelb.yaml](deployments/default/purelb.yaml) | Remove PSP RBAC rules (lines 56-64, 107-115) |

#### Security Hardening (from review):

**Add seccomp profile for `restricted` PSA compliance:**
```yaml
# For allocator
securityContext:
  seccompProfile:
    type: RuntimeDefault
```

**Fix inconsistency:** Set `allowPrivilegeEscalation: false` for lbnodeagent (capabilities don't require it).

**Add namespace template with PSA labels:**
```yaml
# build/helm/purelb/templates/namespace.yaml (NEW FILE)
{{- if .Values.createNamespace }}
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Release.Namespace }}
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
{{- end }}
```

#### Pod Security Standards mapping:
- **Allocator:** Compatible with `restricted` PSA profile (needs seccomp added)
- **LBNodeAgent:** Requires `privileged` PSA profile (NET_ADMIN, NET_RAW, hostNetwork)

#### ⚠️ RED TEAM FIX: Security Hardening Required

**Decision:** Single namespace with mitigations (user preference).

| Fix | File | Change |
|-----|------|--------|
| **Remove Service update from lbnodeagent** | clusterrole-lbnodeagent.yaml | Change `verbs: [get, list, watch]` - remove `update` |
| **Remove Netbox secret from lbnodeagent** | daemonset template | Remove `NETBOX_USER_TOKEN` env var mount |
| **Add seccomp to lbnodeagent** | daemonset template | Add `seccompProfile.type: RuntimeDefault` |

*Note: Memberlist key and Leave timeout fixes deferred - memberlist being removed in subsequent update.*

**Risk:** LOW - PSP already disabled by default. Security fixes are additive.

---

## Phase 2: Moderate Priority Changes

### 2.1 ipMode Field Support

**Why:** Kubernetes 1.30+ has `ipMode` field GA. Setting it to `"VIP"` tells kube-proxy that PureLB manages the IP directly.

#### File to Modify:

| File | Change |
|------|--------|
| [internal/allocator/ingress.go](internal/allocator/ingress.go) | Add `IPMode: &ipModeVIP` to LoadBalancerIngress (line 25) |

```go
// Change from:
v1.LoadBalancerIngress{IP: address.String()}

// To:
ipModeVIP := v1.LoadBalancerIPModeVIP
v1.LoadBalancerIngress{
    IP:     address.String(),
    IPMode: &ipModeVIP,
}
```

**Risk:** VERY LOW - Additive change, older clusters ignore it.

---

### 2.2 Workqueue Typed APIs

**Why:** `NewRateLimitingQueue` is deprecated in favor of `NewTypedRateLimitingQueue[T]`.

#### Files to Modify:

| File | Change |
|------|--------|
| [internal/k8s/k8s.go](internal/k8s/k8s.go) | Define union type, update queue type and constructor (lines 51, 137) |
| [internal/k8s/cr-controller.go](internal/k8s/cr-controller.go) | Update queue type and constructor (lines 64, 103) |

#### Union type approach:

```go
// Define a union type for queue items
type queueItem interface {
    isQueueItem()
}

type svcKey string
func (svcKey) isQueueItem() {}

type synced string
func (synced) isQueueItem() {}

// Use typed queue
queue workqueue.TypedRateLimitingInterface[queueItem]
queue = workqueue.NewTypedRateLimitingQueue[queueItem](
    workqueue.DefaultTypedControllerRateLimiter[queueItem](),
)
```

**Risk:** LOW - Internal change, compile-time type safety.

---

## Phase 3: Minor Priority Changes

### 3.1 Logging Modernization

**Why:** `github.com/go-kit/kit/log` is deprecated (archived 2021). Should use `github.com/go-kit/log`.

#### Files to Modify:

| File | Change |
|------|--------|
| [go.mod](go.mod) | Replace `github.com/go-kit/kit` with `github.com/go-kit/log`, remove `k8s.io/klog v1.0.0` |
| [internal/logging/logging.go](internal/logging/logging.go) | Update imports |
| 18 other files | Change import from `github.com/go-kit/kit/log` to `github.com/go-kit/log` |

**Affected files:**
- `internal/allocator/*.go` (7 files)
- `internal/election/election.go`
- `internal/k8s/*.go` (2 files)
- `internal/local/announcer_local.go`
- `internal/logging/logging.go`
- `cmd/allocator/main.go`
- `cmd/lbnodeagent/controller.go`
- `cmd/lbnodeagent/main.go`

**Risk:** LOW - API compatible, simple find-replace.

---

### 3.2 LoadBalancerIP Documentation

**Why:** `Service.Spec.LoadBalancerIP` is deprecated (K8s 1.24+).

**Already handled:** Code warns users at [allocator.go:275-277](internal/allocator/allocator.go#L275-L277). Just update documentation to recommend `purelb.io/addresses` annotation.

**Risk:** NONE

---

## Implementation Order

```
1. [Phase 1.2] PSP Removal           - Quick win, low risk
2. [Phase 2.1] ipMode Field          - Quick win, very low risk
3. [Phase 1.1] EndpointSlice         - Most complex, highest priority
4. [Phase 3.1] Logging               - Housekeeping
5. [Phase 2.2] Typed Workqueue       - Can defer
```

---

## Testing Requirements

### Unit Tests
- Update [internal/allocator/controller_test.go](internal/allocator/controller_test.go) with EndpointSlice fixtures
- Add dedicated tests for `nodeHasHealthyEndpoint()` with EndpointSlice

### Integration Test Matrix (from review)

| Test Case | Priority | Why Critical |
|-----------|----------|--------------|
| Rolling update with active traffic | P0 | Validates graceful failover |
| Node failure during election | P0 | memberlist Leave() vs crash |
| `ExternalTrafficPolicy=Local` multi-node | P0 | Core functionality |
| EndpointSlice with serving=false, terminating=true | P1 | New discovery/v1 fields |
| Service with >1000 endpoints | P1 | EndpointSlice pagination |
| Multiple EndpointSlices per service (>100 eps) | P1 | Aggregation logic |
| Dual-stack (IPv4/IPv6) services | P1 | Regression |
| Netbox IPAM integration | P1 | External dependency |

### Test Environments
- Cluster 1: 3-node K8s 1.25, single-stack IPv4
- Cluster 2: 5-node K8s 1.27, dual-stack
- Cluster 3: 10-node K8s 1.28, high-scale (1000+ services)

---

## Operational Recommendations (from review)

### Release Strategy

**Decision:** Single release with all changes (user preference).

**Mitigations for single-release approach:**
- Comprehensive testing before release
- Clear rollback documentation
- Feature flag consideration for EndpointSlice if issues arise

### ⚠️ RED TEAM FIX: New Metrics Required

```go
// EndpointSlice metrics
purelb_endpointslice_sync_total
purelb_endpointslice_sync_errors_total
purelb_endpointslice_count              // per service
purelb_endpointslice_cache_age_seconds  // staleness detection

// Orphaned IP detection
purelb_announced_ips_total              // IPs currently announced on this node
purelb_orphaned_ips_total               // IPs on interfaces with no matching service
```

*Note: Memberlist/election metrics deferred - memberlist being removed in subsequent update.*

### ⚠️ RED TEAM FIX: Add IP Reconciliation on Startup

```go
// In announcer_local.go - reconcile on startup
func (a *announcer) ReconcileOrphanedIPs() {
    // List all IPs on kube-lb0 and dummy interfaces
    // Compare against svcIngresses map
    // Remove IPs that have no owner
    // Log warnings for orphaned IPs found
}
```

### Rollback Procedure

```bash
# 1. Rollback Helm release
helm rollback purelb <previous-revision> -n purelb-system

# 2. Force pod restart
kubectl delete pods -n purelb-system -l app.kubernetes.io/name=purelb

# 3. Verify announcements restored
kubectl get svc -A -o json | jq '.items[] | select(.status.loadBalancer.ingress)'
```

---

## Minimum Kubernetes Version

**Kubernetes 1.25+** (required)
- EndpointSlice GA
- Pod Security Admission (replaces PSP)
- No PSP support needed

---

## Implementation Status

All core phases have been implemented:

| Phase | Description | Status |
|-------|-------------|--------|
| **1.1** | Endpoints → EndpointSlice Migration | ✅ Complete |
| **1.2** | PSP Removal + Security Hardening | ✅ Complete |
| **2.1** | ipMode Field Support | ✅ Complete |
| **2.2** | Typed Workqueue Migration | ✅ Complete |
| **3.1** | Logging Modernization (go-kit/log) | ✅ Complete |
| **3.2** | LoadBalancerIP Documentation | Already handled in code |

### Key Changes Made:

1. **EndpointSlice Migration**
   - Added custom indexer `serviceNameIndexName` for efficient slice-to-service lookup
   - Updated event handlers to extract service name from `kubernetes.io/service-name` label
   - Rewrote `nodeHasHealthyEndpoint()` with Ready OR Serving condition support
   - Updated RBAC to use `discovery.k8s.io/endpointslices`

2. **Security Hardening**
   - Deleted PSP templates (`podsecuritypolicy-allocator.yaml`, `podsecuritypolicy-lbnodeagent.yaml`)
   - Added `seccompProfile.type: RuntimeDefault` to both allocator and lbnodeagent
   - Removed `update` verb from lbnodeagent's Services RBAC (read-only)
   - Removed Netbox token mount from lbnodeagent DaemonSet
   - Added `allowPrivilegeEscalation: false` to lbnodeagent

3. **ipMode Field**
   - Added `IPMode: &ipModeVIP` to LoadBalancerIngress in `internal/allocator/ingress.go`

4. **Typed Workqueue**
   - `k8s.go`: Uses union type `queueItem` interface with `svcKey` and `synced` types
   - `cr-controller.go`: Uses `TypedRateLimitingInterface[string]` directly

5. **Logging**
   - Migrated from deprecated `github.com/go-kit/kit/log` to `github.com/go-kit/log`

### Deferred Items (for future work):

- New metrics for EndpointSlice sync and orphaned IP detection
- IP reconciliation on startup (`ReconcileOrphanedIPs`)
- Memberlist-related fixes (being removed in subsequent update)
