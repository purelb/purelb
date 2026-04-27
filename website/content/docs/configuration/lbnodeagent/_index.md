---
title: "LBNodeAgent (Node Configuration)"
description: "Configure PureLB node agent behavior: interface selection, GARP, and address lifetimes."
weight: 20
---

The LBNodeAgent CRD configures how node agents announce addresses. A single LBNodeAgent resource named `default` in `purelb-system` is created during installation and works for most environments.

```sh
kubectl get lbnodeagent -n purelb-system
NAME      AGE
default   5m
```

## Interface Selection

### Local Interface

The `localInterface` field determines which network interface PureLB uses to announce [local addresses]({{< relref "/docs/overview/address-types#local-addresses" >}}).

```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
```

Value | Behavior
------|--------
`default` | Use the interface with the lowest-cost default route (recommended)
Regex pattern | Match interface names (e.g., `eth.*`, `enp0s.*`)

### Dummy Interface

The `dummyInterface` field names the interface for [remote addresses]({{< relref "/docs/overview/address-types#remote-addresses" >}}). PureLB creates it automatically if it doesn't exist.

```yaml
spec:
  local:
    dummyInterface: kube-lb0
```

Default: `kube-lb0`. Change this only if you have a naming conflict.

### Additional Interfaces

The `interfaces` field adds extra interfaces for subnet detection in the [election]({{< relref "/docs/overview/election" >}}). By default, only the interface with the default route is used. If your nodes have multiple network interfaces on different subnets, add them here:

```yaml
spec:
  local:
    localInterface: default
    interfaces:
    - eth1
    - bond0
```

## GARP Configuration

Gratuitous ARP (GARP) notifies network equipment (switches, routers) that an IP-to-MAC binding has changed. This enables faster failover when a local address moves between nodes.

```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
    garpConfig:
      enabled: true
      initialDelay: 100ms
      count: 3
      interval: 500ms
      verifyBeforeSend: true
```

Field | Type | Default | Description
------|------|---------|------------
`enabled` | bool | `true` | Send GARP packets when addresses are added
`initialDelay` | duration | `100ms` | Wait time before first GARP (allows address to be fully configured)
`count` | int (1-10) | `3` | Number of GARP packets to send
`interval` | duration | `500ms` | Time between GARP packets
`verifyBeforeSend` | bool | `true` | Verify the node still owns the address before each GARP

> [!NOTE]
> GARP is recommended for EVPN/VXLAN environments and any L2 network with ARP suppression. In most environments the defaults work well.

## Address Lifetime

LoadBalancer VIP addresses can conflict with CNI plugins (Flannel), DHCP clients, and other systems that inspect address flags to find a node's primary IP. PureLB solves this by giving local addresses a non-permanent lifetime, which clears the `IFA_F_PERMANENT` kernel flag and prevents these systems from selecting VIPs as node addresses.

The `addressConfig` section controls this behavior per interface type.

```yaml
spec:
  local:
    localInterface: default
    addressConfig:
      localInterface:
        validLifetime: 300
        noPrefixRoute: true
      dummyInterface:
        validLifetime: 0
        noPrefixRoute: false
```

### Local Interface Defaults

Field | Default | Description
------|---------|------------
`validLifetime` | `300` | Address validity in seconds. Non-zero values prevent the `IFA_F_PERMANENT` flag.
`preferredLifetime` | Same as `validLifetime` | Preferred lifetime in seconds. Must be <= `validLifetime`.
`noPrefixRoute` | `true` | Prevent kernel from creating a prefix route for the address.

### Dummy Interface Defaults

Field | Default | Description
------|---------|------------
`validLifetime` | `0` (permanent) | Addresses on kube-lb0 are permanent by default.
`preferredLifetime` | `0` (permanent) | Same as `validLifetime`.
`noPrefixRoute` | `false` | Allow kernel to create prefix routes (used for BGP redistribution).

### Why This Matters: Flannel, DHCP, and Address Selection

Many systems inspect the `IFA_F_PERMANENT` flag to identify a node's primary address:

- **Flannel** uses it to find the node IP for VXLAN tunnels. If a VIP has `IFA_F_PERMANENT`, Flannel may select it as the node address, breaking the overlay network.
- **DHCP clients** may avoid renewing an address if they see another permanent address on the interface, leading to lease expiry.
- **Cloud-init and node registration** tools may report the wrong IP to the Kubernetes API server.

PureLB's default of `validLifetime: 300` for local interfaces prevents these issues. The address is auto-renewed well before expiry, so it remains on the interface indefinitely, but without the permanent flag that confuses other software.

## Election Tuning

Election timing is controlled via Helm's `leaseConfig`. See [Election Configuration]({{< relref "/docs/overview/election#configuration" >}}) for the variables and defaults.

## Complete Example

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
    interfaces:
    - eth1
    garpConfig:
      enabled: true
      initialDelay: 100ms
      count: 3
      interval: 500ms
      verifyBeforeSend: true
    addressConfig:
      localInterface:
        validLifetime: 300
        noPrefixRoute: true
      dummyInterface:
        validLifetime: 0
        noPrefixRoute: false
```

## Complete Field Reference

See the [CRD Reference]({{< relref "/docs/reference/crd-reference#lbnodeagent" >}}) for the complete field-by-field specification.
