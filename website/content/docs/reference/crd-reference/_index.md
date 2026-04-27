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

Exactly one of `local`, `remote`, or `netbox` must be specified.

Field | Type | Required | Description
------|------|----------|------------
`local` | ServiceGroupLocalSpec | No | Pool of addresses on the same subnet as nodes
`remote` | ServiceGroupRemoteSpec | No | Pool of addresses on a different subnet (for BGP routing)
`netbox` | ServiceGroupNetboxSpec | No | External IPAM via Netbox

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

### ServiceGroupNetboxSpec

Field | Type | Required | Description
------|------|----------|------------
`url` | string | Yes | Base URL of the Netbox API
`tenant` | string | Yes | Netbox tenant name
`aggregation` | string | No | Override address mask (`"default"` or `"8"`-`"128"`)

### AddressPool

Field | Type | Required | Description
------|------|----------|------------
`pool` | string | Yes | Address range: CIDR (`192.168.1.240/29`) or range (`192.168.1.240-192.168.1.250`)
`subnet` | string | Yes | CIDR of the containing network (`192.168.1.0/24`). All pool addresses must be within this subnet.
`aggregation` | string | No | Address mask override. `"default"` uses subnet mask. Explicit values like `"/32"` or `"/128"` create host routes.

### ServiceGroupStatus

Field | Type | Description
------|------|------------
`allocatedCount` | int | Number of IP addresses currently allocated from this pool

---

## LBNodeAgent

**API Version:** `purelb.io/v2`
**Short Names:** `lbna`, `lbnas`
**Scope:** Namespaced

### LBNodeAgentSpec

Field | Type | Description
------|------|------------
`local` | LBNodeAgentLocalSpec | Local announcer configuration

### LBNodeAgentLocalSpec

Field | Type | Default | Description
------|------|---------|------------
`localInterface` | string | `"default"` | Interface for local address announcement. `"default"` uses the interface with the default route. Regex patterns match interface names.
`dummyInterface` | string | `"kube-lb0"` | Dummy interface for remote addresses. Created automatically if it doesn't exist.
`interfaces` | []string | | Additional interfaces for subnet detection in election
`garpConfig` | GARPConfig | | Gratuitous ARP configuration
`addressConfig` | AddressConfig | | Address lifetime and flag configuration

### GARPConfig

Field | Type | Default | Description
------|------|---------|------------
`enabled` | bool | `true` | Send GARP packets when addresses are added
`initialDelay` | string (duration) | `"100ms"` | Wait time before first GARP
`count` | int (1-10) | `3` | Number of GARP packets to send
`interval` | string (duration) | `"500ms"` | Time between GARP packets
`verifyBeforeSend` | bool | `true` | Verify election win before each GARP

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
`families` | []string | Required | Address families: `"ipv4-unicast"`, `"ipv6-unicast"`
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
`timers.holdTime` | int | BGP hold time (seconds)
`timers.keepaliveInterval` | int | Keepalive interval (seconds)
`transport.passiveMode` | bool | Wait for peer to initiate
`nodeSelector` | LabelSelector | Limit which nodes peer with this neighbor

---

## BGPNodeStatus

**API Version:** `bgp.purelb.io/v1`
**Scope:** Namespaced

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
