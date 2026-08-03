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

The `localInterface` field determines which network interfaces PureLB uses to announce [local addresses]({{< relref "/docs/overview/address-types#local-addresses" >}}). It governs **both** sides of announcement: which interface the address is added to, **and** which subnets the node advertises in the [election]({{< relref "/docs/overview/election" >}}) — a node only becomes a candidate for an address whose subnet its selected interfaces carry.

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
Regex pattern | Match interface names (e.g., `^eth[12]$`)

> [!WARNING]
> Regex matching is **unanchored**: the pattern `eth` matches `veth1a2b3c` and every other name containing "eth". Always anchor your pattern (`^eth1$`, `^(eth1|eth2)$`) — an under-anchored regex on a busy node pulls pod and bridge interfaces into subnet detection, and the selected interface list is logged at info level when it changes so this is visible in the agent logs.

> [!WARNING]
> An **invalid regex** makes the node announce nothing: the agent emits a Warning Event on the LBNodeAgent resource, sets `purelb_lbnodeagent_selector_state{state="invalid"}` to 1, empties its lease subnets within a few seconds, and withdraws its addresses so they migrate to healthy nodes. A typo in a cluster-wide (catch-all) resource therefore affects **every node at once** — treat edits to it as change-window operations.

### Dummy Interface

The `dummyInterface` field names the interface for [remote addresses]({{< relref "/docs/overview/address-types#remote-addresses" >}}). PureLB creates it automatically if it doesn't exist.

```yaml
spec:
  local:
    dummyInterface: kube-lb0
```

Default: `kube-lb0`. Change this only if you have a naming conflict.

### Additional Interfaces

The `interfaces` field adds extra interfaces by exact name, on top of whatever `localInterface` selects. Each listed interface participates in **both** subnet detection for the [election]({{< relref "/docs/overview/election" >}}) and address announcement: when the primary selection (default route or regex) doesn't resolve an address, the listed interfaces are tried **in the order written** and the first one whose subnet contains the address wins. Interfaces that don't exist on a given node are skipped silently, so one cluster-wide resource works across heterogeneous nodes.

```yaml
spec:
  local:
    localInterface: default
    interfaces:
    - eth1
    - bond0
```

## Node Selection (nodeSelector)

By default an LBNodeAgent resource applies to every node. The optional `nodeSelector` (a standard Kubernetes label selector) scopes it to matching nodes, enabling per-node-group configuration — the natural pattern for clusters with heterogeneous NIC naming or a few special multi-NIC nodes: one catch-all `default` resource plus a scoped override.

```yaml
# Catch-all for the cluster
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
---
# Override for the dual-homed node(s)
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: dual-homed
  namespace: purelb-system
spec:
  nodeSelector:
    matchLabels:
      example.com/dual-homed: "true"
  local:
    localInterface: default
    interfaces:
    - eth2
```

**Precedence** when multiple resources match a node: a resource with a specific selector beats a catch-all (`nodeSelector` absent **or** empty `{}` — both match all nodes); remaining ties are broken by namespace/name sort order, first wins. Ignored matches are logged and receive a Warning Event, visible in `kubectl describe lbnodeagent`.

**Label evaluation** follows the Kubernetes scheduling convention: labels are read when configuration is delivered, not watched continuously. A label-only change takes effect on the next LBNodeAgent/ServiceGroup change, on the periodic re-sync (within about 10 minutes), or on agent restart. If no resource matches a node, that node announces nothing and advertises no subnets (`selector_state{state="deselected"}`).

> [!NOTE]
> With the `NodeRestriction` admission plugin, kubelets can still set most labels on their own Node object. For selector keys that must not be node-settable, use the `node-restriction.kubernetes.io/` prefix.

**Upgrade and rollback ordering** matters once scoped resources exist:

- Upgrade the CRD and **all** agents before creating any resource that uses `nodeSelector`. Older CRDs silently prune the field (turning a scoped resource into a second catch-all), and older agent binaries ignore it and adopt the first Local resource in arbitrary order — a one-node override could be applied cluster-wide.
- Delete scoped resources **before** rolling the agent binary back, for the same reason.

## Multi-NIC Nodes: Routing Requirements

Announcing a VIP on a NIC that does **not** hold the node's default route works at the PureLB layer, but the host's routing must cooperate when clients are outside the VIP's subnet:

- Reply traffic follows the default route out the *other* NIC (asymmetric routing). With strict reverse-path filtering (`rp_filter=1`, the RHEL-family default) the kernel drops the inbound connection on the VIP's NIC before PureLB's announcement even matters.
- Fix it with source-based policy routing — route traffic *from* the VIP's subnet via that NIC's gateway — or relax `rp_filter` to loose (`2`) on the announcing NIC. Deployments whose clients are all on the VIP's own subnet are unaffected.

Example (netplan, second NIC `eth2` on `172.30.250.0/24`, gateway `.1`; keeps DHCP and demotes eth2's default route so eth1 stays primary):

```yaml
network:
  version: 2
  ethernets:
    eth2:
      dhcp4: true
      dhcp4-overrides: {route-metric: 200, use-dns: false}
      routes:
      - {to: default, via: 172.30.250.1, table: 250}
      routing-policy:
      - {from: 172.30.250.0/24, to: 172.24.0.0/16, table: 254, priority: 990}   # pods stay on main
      - {from: 172.30.250.0/24, to: 172.30.0.0/16, table: 254, priority: 991}   # cluster nets stay on main
      - {from: 172.30.250.0/24, table: 250, priority: 1000}                      # everything else via eth2
```

## Known Limitations

- **DHCPv6-managed NICs**: DHCPv6 (IA_NA) assigns `/128` addresses, so subnet detection yields a subnet containing only the node itself — the NIC can never become an election candidate for pool addresses. Use SLAAC or a static address with the real prefix length; the agent logs at debug level when a v6 address collapses to `/128`.
- **`validLifetime: 0` is unsupported for local announcements**: it makes IPv6 VIPs permanent, defeating the deprecated-address marking that both CNI plugins (Flannel) and the election's self-interference filtering rely on.
- **Primary-address loss**: if a NIC loses its real (e.g. DHCP) address while a VIP remains, Linux `promote_secondaries` promotes the VIP and the node can keep advertising that subnet from its own VIP until the VIP's lifetime expires. This window is narrow (address loss without link loss) and bounded by the address lifetime (default 300s).
- **Startup transient**: for the first seconds after an agent starts, before configuration arrives, the node advertises its default-interface subnets. A node that is deliberately deselected by every `nodeSelector` may therefore briefly win an election it won't serve; this self-corrects at the first configuration delivery.

## GARP Configuration

Gratuitous ARP (GARP) notifies network equipment (switches, routers) that an IP-to-MAC binding has changed. This enables faster failover when a local address moves between nodes. For IPv6 addresses the same configuration sends **unsolicited Neighbor Advertisements** — the NDP counterpart of GARP — with identical delay/count/interval/verify semantics; without them a moved IPv6 VIP converges only via neighbor-cache timeouts (typically 5–30 seconds).

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
