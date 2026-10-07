---
title: "CRD Reference"
description: "Complete field reference for all PureLB Custom Resource Definitions."
weight: 10
---

## ServiceGroup

**API Version:** `purelb.io/v2`
**Short Names:** `sg`, `sgs`
**Scope:** Namespaced

### ServiceGroupSpec

Exactly one of `local`, `remote`, or `external` must be specified.

Field | Type | Required | Description
------|------|----------|------------
`local` | ServiceGroupLocalSpec | No | Pool of addresses on the same subnet as nodes
`remote` | ServiceGroupRemoteSpec | No | Pool of addresses on a different subnet (for BGP routing)
`external` | ServiceGroupExternalSpec | No | Addresses managed by an external IPAM system via a sidecar
`namespaces` | []string | No | Service namespaces this ServiceGroup serves. Empty means all. Several ServiceGroups may serve one namespace
`enforceNamespaces` | bool | No | Make `namespaces` a boundary rather than a default. Requires a non-empty `namespaces`
`namespaceDefault` | bool | No | Which ServiceGroup an unannotated Service gets, where several serve one namespace. Requires a non-empty `namespaces`

Exactly one of `local`, `remote` or `external` must be specified.

### ServiceGroupLocalSpec

Field | Type | Default | Description
------|------|---------|------------
`v4pool` | AddressPool | | Single IPv4 pool (shorthand for `v4pools` with one entry)
`v6pool` | AddressPool | | Single IPv6 pool (shorthand for `v6pools` with one entry)
`v4pools` | []AddressPool | | Array of IPv4 pools
`v6pools` | []AddressPool | | Array of IPv6 pools
`skipIPv6DAD` | bool | `false` | Disable IPv6 Duplicate Address Detection
`multiPool` | bool | `false` | Allocate one IP per range per family with active nodes
`balancePools` | bool | `false` | Allocate to range with fewest IPs in use (mutually exclusive with `multiPool`)

### ServiceGroupRemoteSpec

Field | Type | Default | Description
------|------|---------|------------
`v4pool` | AddressPool | | Single IPv4 pool
`v6pool` | AddressPool | | Single IPv6 pool
`v4pools` | []AddressPool | | Array of IPv4 pools
`v6pools` | []AddressPool | | Array of IPv6 pools
`multiPool` | bool | `false` | Allocate one IP per range per family with active nodes
`balancePools` | bool | `false` | Allocate to range with fewest IPs in use (mutually exclusive with `multiPool`)

### ServiceGroupExternalSpec

Addresses come from an external IPAM system through a sidecar in the allocator pod. See [External IPAM (Sidecar)]({{< relref "/docs/configuration/external-ipam" >}}).

Field | Type | Required | Description
------|------|----------|------------
`provider` | string | Yes | IPAM provider name. Display only (shown in `.status.ipam`); not checked against the sidecar
`socket` | string | No | Absolute path of the sidecar's Unix socket in the allocator pod. Default `/var/run/purelb/ipam.sock`
`announce` | string | Yes | How addresses from this pool are announced: `local` (node interface) or `remote` (`kube-lb0`, for BGP)

### AddressPool

Field | Type | Required | Description
------|------|----------|------------
`pool` | string | Yes | Address range: CIDR (`192.168.1.240/29`) or range (`192.168.1.240-192.168.1.250`)
`subnet` | string | Yes | CIDR of the containing network (`192.168.1.0/24`). All pool addresses must be within this subnet.
`aggregation` | string | No | Address mask override. `"default"` uses subnet mask. Explicit values like `"/32"` or `"/128"` create host routes.

### ServiceGroupStatus

Field | Type | Description
------|------|------------
`announce` | string | Announcement mechanism: `Local` (node interface) or `Remote` (`kube-lb0`, advertised via BGP)
`ipam` | string | `Cluster` when PureLB allocates, or the external provider's name
`addresses` | []string | Human-readable summary of the pool's address scope
`allocatedIPv4` | int64 | IPv4 addresses currently allocated
`allocatedIPv6` | int64 | IPv6 addresses currently allocated
`availableIPv4` | int64 | IPv4 addresses still available. Absent when capacity is not knowable
`availableIPv6` | int64 | IPv6 addresses still available. Absent when capacity is not knowable
`boundNamespaces` | []string | The namespace list the allocator parsed from `spec.namespaces`

A blank status means PureLB rejected the spec — check the ServiceGroup's events.

`boundNamespaces` confirms what the allocator **read**, which is not the same as what the API server accepted, and it is not a report of what is being *enforced*: it reflects neither `enforceNamespaces` nor an unresolved `namespaceDefault`. It also cannot detect a pruned CRD, because `spec.namespaces` and this field ship in the same CRD and a stale CRD removes both — to check for pruning, apply the field and read the object back.

---

## LBNodeAgent

**API Version:** `purelb.io/v2`
**Short Names:** `lbna`, `lbnas`
**Scope:** Namespaced

### LBNodeAgentSpec

Field | Type | Description
------|------|------------
`nodeSelector` | metav1.LabelSelector | Limits which nodes this resource applies to. Absent or empty = all nodes (catch-all). When multiple resources match a node: specific selector beats catch-all, then namespace/name sort order. See [Node Selection]({{< relref "/docs/configuration/lbnodeagent#node-selection-nodeselector" >}}).
`local` | LBNodeAgentLocalSpec | Local announcer configuration

### LBNodeAgentLocalSpec

Field | Type | Default | Description
------|------|---------|------------
`localInterface` | string | `"default"` | Interface for local address announcement **and** election subnet detection. `"default"` uses the interface with the default route. Regex patterns match interface names (unanchored — anchor your pattern).
`dummyInterface` | string | `"kube-lb0"` | Dummy interface for remote addresses. Created automatically if it doesn't exist.
`interfaces` | []string | | Additional interfaces, by exact name, for election subnet detection and announcement. Tried in listed order; missing names are skipped.
`garpConfig` | GARPConfig | | Gratuitous announcement configuration (GARP for IPv4, unsolicited Neighbor Advertisement for IPv6)
`addressConfig` | AddressConfig | | Address lifetime and flag configuration
`addressGuard` | AddressGuardConfig | | Filter traffic to LoadBalancer addresses to their Service ports. Absent means off. See [Address Guard]({{< relref "/docs/configuration/address-guard" >}})

### AddressGuardConfig

Field | Type | Default | Description
------|------|---------|------------
`mode` | string | `"enforce"` | `enforce` drops traffic to a VIP that isn't for one of its Service ports; `monitor` only counts it
`hook` | string | `"tcx"` | `tcx` (TC ingress) or `xdp` (native XDP on links whose driver supports it; others use tcx)
`failurePolicy` | string | `"closed"` | When the guard can't run on a node: `closed` makes the node announce no addresses until it can; `open` keeps announcing, unfiltered
`allowedProtocols` | []int | | IP protocol numbers (0-255, at most 32) allowed to reach VIPs besides TCP, UDP, SCTP and ICMP, e.g. `47` (GRE). Port-filtered and ICMP protocols and IPv6 extension-header numbers are rejected
`extraInterfaces` | []string | | Interfaces to guard in addition to the automatic set (physical NICs, bonds, default-route interfaces). At most 64; missing names are skipped
`excludeInterfaces` | []string | | Interfaces not to guard. VIP traffic arriving on them is not filtered. At most 64; a name can't be in both lists

### GARPConfig

Applies to both IPv4 (gratuitous ARP) and IPv6 (unsolicited Neighbor Advertisement) announcements.

Field | Type | Default | Description
------|------|---------|------------
`enabled` | bool | `true` | Send announcement packets when addresses are added
`initialDelay` | string (duration) | `"100ms"` | Wait time before first packet
`count` | int (1-10) | `3` | Number of packets to send
`interval` | string (duration) | `"500ms"` | Time between packets
`verifyBeforeSend` | bool | `true` | Verify election win before each packet

### AddressConfig

Field | Type | Description
------|------|------------
`localInterface` | InterfaceAddressConfig | Configuration for addresses on the local interface
`dummyInterface` | InterfaceAddressConfig | Configuration for addresses on the dummy interface

### InterfaceAddressConfig

Field | Type | Default (local) | Default (dummy) | Description
------|------|-----------------|-----------------|------------
`validLifetime` | int (seconds) | `300` | `0` (permanent) | Address validity. Non-zero prevents `IFA_F_PERMANENT` flag. Min when non-zero: `60`.
`preferredLifetime` | int (seconds) | Same as `validLifetime` | `0` | Preferred lifetime. Must be <= `validLifetime`.
`noPrefixRoute` | bool | `true` | `false` | Prevent kernel from creating a prefix route for the address.

### LBNodeAgentStatus

Field | Type | Description
------|------|------------
`activeLeases` | int | Number of active election Leases this node holds

---

## BGPConfiguration

**API Version:** `bgp.purelb.io/v1`
**Short Name:** `bgpconfig`
**Scope:** Namespaced

### Global

Field | Type | Default | Description
------|------|---------|------------
`asn` | int32 | Required | Local Autonomous System Number
`routerID` | string | Auto-detect | BGP router identifier. Empty for auto-detection, explicit IP, or template variable (`${NODE_IP}`)
`listenPort` | int32 | `179` | BGP listen port
`families` | []string | `ipv4-unicast`, `ipv6-unicast` | Global address families. Optional: omit it to keep both. Lowercase, case-sensitive
`listenAddresses` | []string | | IPs to listen on
`gracefulRestart` | object | | Graceful restart configuration

### netlinkImport

Field | Type | Default | Description
------|------|---------|------------
`enabled` | bool | `false` | Enable kernel route import into BGP
`interfaceList` | []string | | Interface glob patterns (e.g., `"kube-lb0"`)

### Neighbors

Field | Type | Description
------|------|------------
`config.neighborAddress` | string | Peer IP address (required)
`config.peerAsn` | int32 | Peer's ASN (required)
`config.description` | string | Human-readable description
`config.authPasswordSecretRef` | object | Reference to Secret with BGP auth password
`afiSafis` | []object | Per-family configuration (family, enabled)
`timers.config.holdTime` | int | BGP hold time (seconds). Changing it resets the session
`timers.config.keepaliveInterval` | int | Keepalive interval (seconds). Changing it resets the session
`transport.passiveMode` | bool | Wait for peer to initiate
`nodeSelector` | LabelSelector | Nodes that peer with this neighbor, matched on node labels. Used to give each subnet its own peer in one configuration (see [Peers on multiple subnets](../../configuration/bgp/#peers-on-multiple-subnets)). Omitted: every node peers with it

---

## BGPNodeStatus

**API Version:** `bgp.purelb.io/v1`
**Scope:** Cluster

Read-only status resource written by k8gobgp per node.

Field | Type | Description
------|------|------------
`nodeName` | string | Kubernetes node name
`asn` | int32 | Local ASN
`routerID` | string | Router ID used
`healthy` | bool | All neighbors established and no failures
`neighborCount` | int | Number of configured neighbors
`neighbors` | []object | Per-neighbor state (address, state, ASN, prefixes sent/received, lastError, sessionUpSince)
`lastUpdated` | timestamp | Last status write
`conditions` | []Condition | Kubernetes-style condition objects
