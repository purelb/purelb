---
title: "Egress"
description: "Give pods a stable source address from a PureLB pool, on local, routed and external pools, IPv4 and IPv6."
weight: 55
---

Egress gives selected pods a stable source address from a PureLB pool. An external firewall, partner or API can then allow a fixed address instead of every node's.

The address is allocated and placed like a LoadBalancer address. The pod's node translates selected traffic to it, using a range of source ports (a *slice*) of its own. Traffic leaves directly when that node holds the address, and otherwise through a Geneve tunnel to a node that does. Replies come back by slice, with no per-flow state on the holder.

Egress is idle until an LBNodeAgent enables it. With it off on a node, nothing changes there: no tunnel, nft table, routing rules, pins or egress informers, and the CNI configuration isn't touched.

## Requirements

- Kubernetes 1.31 or newer, for every PureLB install.
- A Linux kernel with [CVE-2025-40183](https://nvd.nist.gov/vuln/detail/CVE-2025-40183) fixed: 6.6.113+, 6.12.54+, 6.17.4+, 6.18+, or a distribution backport. Older kernels leak memory on every tunnelled packet. A kernel older than 6.6 reports `DatapathCompatible=False KernelTooOld`.
- The tunnel port (`tunnelPort`, UDP) open between nodes. It must differ from the CNI's own Geneve port.
- `net.ipv4.conf.all.forwarding=1`, and for IPv6 `net.ipv6.conf.all.forwarding=1`. `arp_announce=2` on the interfaces local pools use.
- The node agent's capabilities stay `NET_ADMIN`, `NET_RAW` and `BPF`. Egress adds hostPaths for `/sys/fs/bpf/purelb`, `/var/lib/purelb` and `/run/purelb`, and a token-less `purelb-cni` DaemonSet owns the CNI directories. On RKE2 and k3s set its paths in the chart's values.

`kubectl purelb validate --live` checks these on each node.

### MTU

Tunnelled traffic carries 36 more bytes over an IPv4 underlay and 56 over IPv6, plus 14 in L2 mode. Prefer an underlay MTU that much larger than the pool interface's, so full-size packets fit. Where you can't, PureLB computes the path MTU, clamps TCP MSS on both paths, and answers oversize packets with an ICMP error.

## Enabling it

Enable egress on the nodes, allow it on a ServiceGroup, then create a policy:

```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  egress:
    enabled: true
---
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  egress:
    allowed: true
  # ... your pools
---
apiVersion: purelb.io/v2
kind: Egress
metadata:
  name: sip
  namespace: voice
spec:
  serviceGroup: default
  podSelector:
    matchLabels:
      app: sip
  ipFamilyPolicy: PreferDualStack
  failurePolicy: FailClosed
  destinations:
    include:
    - cidrs: ["0.0.0.0/0", "::/0"]
```

Destinations inside the cluster's own networks (pods, Services, nodes and the API server) never use egress. PureLB finds these from each Node's `podCIDRs` and the node's routes. Add ranges it can't see to `spec.egress.podNetworks` on the LBNodeAgent, and destinations to leave alone to `excludedNetworks`.

A `ClusterEgress` is the same policy, cluster-scoped, for an administrator. It selects namespaces as well as pods and ranks above every `Egress`.

### Failure policy

While a selected pod's egress isn't ready, `FailClosed` drops its traffic leaving the cluster and `FailOpen` lets it leave from the pod's own address. The plugin waits up to 25 seconds for the pod's slice before letting it start. A `FailClosed` pod doesn't start at all on a node that can't run egress, or while a node's first selected pods wait for its eBPF stage: the CNI plugin fails its ADD and kubelet retries.

### Addresses and ports

`affinity.mode` | What pods share
---|---
`Hash` (default) | One address, with each node given its own slices of source ports. Protocols without ports aren't supported on it.
`PerPod` | One address per StatefulSet ordinal (`identity: StatefulSetOrdinal`), with every port and protocol. Conntrack helpers need this mode.
`Inherit` | The address of the Service in `serviceRef`, placed wherever the Service's is. The Service keeps owning the address, and its ports keep working. The group must set `allowSharedServiceAddress`.

`ports.sliceSize` sets the ports per slice: 64 to 16,384, a power of two. An address has floor((65536 - 1024) / sliceSize) slices, less any reserved and Service ports, and less slices still draining (at most 5 minutes). `ports.reserved` keeps a pod's own source port on the address.

## Local and routed pools

On a **local** pool, one node holds the address, chosen by the same Lease election as a LoadBalancer address. Set `preferPodNodes` to prefer nodes with pods using it.

On a **routed** pool, every live node matching `announcerNodeSelector` advertises the address through BGP and serves its replies. With the pool's `aggregation` shorter than a host route, every one of them advertises the pool's aggregate and serves every address in it. Set `announcerNodeSelector` to the nodes that peer.

{{< hint warning >}}
Don't enable BFD on routed egress yet. k8gobgp's BFD doesn't recover after a session drop and then flaps BGP ([purelb/gobgp-netlink#65](https://github.com/purelb/gobgp-netlink/issues/65)). `kubectl purelb validate` warns about it. Without BFD, a failed node's route is withdrawn when BGP notices, in about 8 to 9 seconds at the timers tested.
{{< /hint >}}

## CNIs

PureLB finds out how each node's traffic flows with a probe at the first selected pod after the agent starts, and runs one of two stages:

- The **netfilter stage** tags pods in eBPF and translates in nft, where the pod's traffic reaches the host's forward path.
- The **eBPF stage** translates in eBPF on the NIC with PureLB's own NAT table, where the CNI redirects pod traffic past netfilter.

CNI | Stage | Tested
---|---|---
Flannel | Netfilter | Yes
Calico, standard dataplane | Netfilter | Yes
Calico eBPF, `natOutgoing` off | eBPF | Yes
Calico eBPF, `natOutgoing` on | Netfilter, by source address | Measured on one test, not the full suite
Cilium, eBPF host routing with veth or netkit | eBPF | Yes
Antrea | Netfilter | Selected cases
kube-router, Canal | Not tested | No

### Cilium

Cilium owns its CNI configuration and rewrites it, so `purelb-cni` can't add the plugin. Add it through Cilium's own configuration: a ConfigMap holding Cilium's conflist with PureLB's plugin appended, named by Cilium's Helm `cni.configMap`. It is coupled to Cilium's version. **Enable egress before `cni.configMap` names the plugin**, or no pod can start: the plugin fails ADD while there is no agent for it to ask.

### Calico

- On the eBPF dataplane, the selected pods' pools on a node must share `natOutgoing`. A node that can't translate reports `NATOutgoingRequired`.
- Calico's BGP and k8gobgp both want port 179. Either turn off Calico's BGP and use VXLAN, or don't use PureLB's BGP there (local pools only).

## What the eBPF stage doesn't do

- SCTP on a slice address: `ProtocolUnsupported`. PerPod addresses carry it.
- Conntrack helpers: `HelpersUnsupported`.
- Its flows aren't in `conntrack -L`, and `flowLog` is off there. `kubectl purelb egress dump` shows its NAT flow count.

## Conntrack helpers

`helpers` (FTP, SIP, H323) work on PerPod addresses. Each needs the kernel's `nf_conntrack_<h>`, loaded on demand, and `nf_nat_<h>`, which nothing loads for you: add it to your node's module list. H.323 is IPv4 only. A helper that can't run reports `HelpersUnsupported`.

## Flow log

`spec.egress.flowLog: true` on the LBNodeAgent logs each flow's start and end at Info (`op=egressFlow`). Byte and packet counts need `net.netfilter.nf_conntrack_acct=1`, and durations `nf_conntrack_timestamp=1`; the agent doesn't change either. With `nf_conntrack_events=2`, a flow created before the log started has no end event.

## Tenancy

`spec.egress.tenancy` on a ServiceGroup limits what namespaces can do: `maxEgressesPerNamespace`, `priorityCeiling` and `allowClusterEgress`. `statusDetail: Redacted` hides nodes, slices and holders from a policy's status, sends events naming nodes to the PureLB namespace, and leaves the group's addresses out of per-address metrics. `onRevocation` says what happens when a policy stops being allowed.

## Encryption

Egress's tunnel doesn't go through WireGuard or IPsec between nodes. A node that encrypts reports `UnderlayEncrypted=False UnderlayUnencrypted`, and egress keeps working. Keep this in mind if pod traffic must stay encrypted until it leaves the cluster.

## Turning it off

Set `spec.egress.enabled: false` on the LBNodeAgent. Each node removes the CNI chain entry, the plugin, its programs and pins, nft table, rules and tunnel, resetting egress's flows. The chart's pre-delete hook does this at uninstall, and waits for every node.

If a node can't finish, for instance because its agent is gone:

- `kubectl purelb egress cleanup --node <node>` removes egress from it, running a privileged pod there.
- `kubectl purelb egress release-finalizers <namespace/name>` frees a policy stuck deleting without the allocator. It skips quarantine and any external IPAM's release.

A forgotten uninstall doesn't brick a node: once the snapshot is older than `snapshotMaxAge` (1 h by default) and no agent answers, the plugin lets pods start.

Downgrading is refused by the chart while a node reports programmed egress. A manifest install, or a downgrade to 0.16.x, can't refuse: disable egress first.

## Known limits

- At boot a node's CNI can start pods before the plugin is chained. Those pods are reported per pod (`EgressSandboxUnrecorded`, `purelb_egress_cni_unrecorded_pods`) and attached. On Antrea this window measured about 7 seconds.
- A local-pool address moves off with its node's Lease when that agent restarts, and back with the next agent: two takeovers of about 1 to 3 seconds each.

See [Monitoring]({{< relref "/docs/operations/monitoring" >}}) for the alerts and their runbooks, and [`kubectl purelb egress`]({{< relref "/docs/reference/kubectl-plugin-reference" >}}#egress-trace) to trace a flow.
