---
title: "Address Guard"
description: "Only Service ports reach a LoadBalancer address: stop host services such as sshd and the kubelet answering on VIPs."
weight: 50
---

PureLB puts each LoadBalancer address (VIP) on a node interface: the node's NIC for a local pool, `kube-lb0` on every node for a remote pool. That makes the VIP an address of the host itself, so **every service on the host that listens on all addresses also answers on the VIP**: sshd, the kubelet (10250), anything using a `hostPort`, and, with kube-proxy in iptables mode, every NodePort. kube-proxy forwards only the Service's own ports; it does nothing about the rest.

The address guard closes that. It is a small eBPF program on each node's uplinks that, for packets addressed to a PureLB VIP, lets through only:

* the Service's ports (TCP, UDP and SCTP; the Service `port`, never the NodePort). When several Services share an address, the ports of all of them;
* ICMP needed for the network to work: echo (ping), destination unreachable (including IPv4 "fragmentation needed", which path MTU discovery depends on), time exceeded, parameter problem, IPv6 packet too big, and IPv6 neighbor solicitation/advertisement. An error message (unreachable, packet too big, time exceeded, parameter problem) must be about the VIP's own traffic: it quotes the packet that caused it, and the quoted packet's source must be the VIP. One quoting any other address, such as the node's own, is forged, and dropped (`icmp_denied`), since the kernel would act on the quote: lowering the path MTU to the quoted destination, or reporting an error to the socket it matches;
* IP protocols you list in `allowedProtocols`.

Everything else addressed to the VIP is dropped and counted. Traffic to the node's own addresses is not touched. IPv4 and IPv6 are handled alike.

The guard is **off unless configured**.

## Requirements

* Linux kernel **6.6 or newer** on the nodes. On older kernels the guard does not load at all, and with the default `failurePolicy: closed` such a node announces no addresses (see [Failure behaviour](#failure-behaviour)).
* The lbnodeagent container needs the `BPF` and `NET_ADMIN` capabilities. The manifests and Helm chart include both; if you override the Helm `containerSecurityContext`, keep them in `capabilities.add`.
* A container runtime whose default seccomp profile allows `bpf()` when the container has `CAP_BPF` (containerd 1.7 does).

`kubectl purelb validate` checks the kernel version of the nodes the guard is configured on, the agent's capability, and whether the installed CRD knows the `addressGuard` field.

## Rolling it out

Start in **monitor** mode: the guard counts what it would drop and drops nothing.

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
    addressGuard:
      mode: monitor
```

The guard's program is loaded only when `lbnodeagent` starts with the guard configured, so after adding `addressGuard`, restart the agents:

```sh
kubectl -n purelb-system rollout restart daemonset/lbnodeagent
kubectl -n purelb-system rollout status daemonset/lbnodeagent
```

Until they restart, nothing is filtered and each node reports `purelb_address_guard_restart_required` 1 (see [Enabling and disabling](#enabling-and-disabling)). Once it is loaded, every other change — `mode`, `hook`, `failurePolicy`, `allowedProtocols`, the interface lists — applies live. Check that it is on:

```sh
curl -s http://<node-ip>:7472/metrics | grep -E '^purelb_address_guard_(loaded|attached|restart_required)'
# loaded 1, an attached line per uplink, restart_required 0
```

Then look at what enforce would block:

```sh
# per VIP, on each node
curl -s http://<node-ip>:7472/metrics | grep 'purelb_address_guard_vip_packets_total{action="would_drop"'
```

Anything listed there is traffic reaching a VIP on a port that isn't one of its Services' ports. If something legitimate shows up, add the port to the Service (or, for a non-port protocol, to `allowedProtocols`). When you are satisfied, set `mode: enforce`.

To try enforce on a few nodes first, give them their own LBNodeAgent with a `nodeSelector`. A specific selector takes precedence over the catch-all `default`:

```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: guard-canary
  namespace: purelb-system
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: node-3
  local:
    localInterface: default
    dummyInterface: kube-lb0
    addressGuard:
      mode: enforce
```

If the guard isn't running on those nodes yet, restart only their agents, so that only they load it:

```sh
kubectl -n purelb-system delete pod -l component=lbnodeagent --field-selector spec.nodeName=node-3
```

`kubectl purelb status` shows how many nodes run each mode, and `kubectl purelb inspect <namespace>/<service>` shows what the guard allows on each of a Service's addresses.

### Check that it is on

Helm does not upgrade CRDs. If the installed LBNodeAgent CRD predates the address guard, the API server silently drops `addressGuard` from your resource and the guard stays off. After configuring it, check both the resource and the nodes:

```sh
kubectl get lbnodeagent default -n purelb-system -o jsonpath='{.spec.local.addressGuard}'
curl -s http://<node-ip>:7472/metrics | grep -E '^purelb_address_guard_(attached|restart_required)'
```

No `attached` lines with `restart_required` 1 means the agents haven't been restarted since the guard was configured. [`kubectl purelb guard`](#kubectl-purelb-guard-what-each-node-is-doing) shows this for every node at once.

## Configuration

All fields live under `spec.local.addressGuard`.

| Field | Values | Default | |
|---|---|---|---|
| `mode` | `enforce`, `monitor` | `enforce` | `monitor` counts would-be drops and drops nothing. |
| `hook` | `tcx`, `xdp` | `tcx` | Where the program attaches. See [Hooks](#hooks). |
| `failurePolicy` | `closed`, `open` | `closed` | What a node does when the guard can't do its job. See [Failure behaviour](#failure-behaviour). |
| `allowedProtocols` | IP protocol numbers, at most 32 | none | Non-port protocols allowed to reach VIPs, e.g. `[47]` for GRE, `[50]` for ESP. These can't be listed: 6, 17 and 132 (TCP, UDP and SCTP are filtered by port), 1 and 58 (ICMP is filtered by type), and 0, 43, 44 and 60 (IPv6 extension headers, which the guard walks past rather than treating as protocols). |
| `extraInterfaces` | interface names, at most 64 | none | Guard these links as well as the automatic set. |
| `excludeInterfaces` | interface names, at most 64 | none | Don't guard these links. **VIP traffic arriving on an excluded link is not filtered.** |

An interface can't be in both lists, and `lo` can't be guarded.

### Attachment

The guard attaches where traffic from outside the cluster enters a node:

* every physical NIC,
* every bond (in place of its member NICs),
* the interfaces holding the IPv4 and IPv6 default routes, whatever kind they are.

That covers VLANs, bridges built on a NIC, and macvlan too: the physical NIC sees their packets first. It deliberately leaves out tunnels (`flannel.1`, `tunl0`, WireGuard), CNI bridges such as `cni0`, `lo`, dummies and pod veths. Anything that can reach a node through those can already reach the node's own address, and the host services on it, directly; filtering the VIP there would protect nothing.

The guard follows interfaces as they come and go. Use `extraInterfaces` for an ingress path the automatic rule can't know about, for instance clients arriving through a WireGuard or GRE tunnel to an edge router. Names that don't exist on a node are skipped, so one list can serve nodes with different interfaces; use a `nodeSelector` when nodes need different lists.

The guard attaches only to links whose packets it knows how to parse: Ethernet links (physical NICs, bonds, VLANs, veths, TAP, `gretap`), and links with no link-layer header (`ipip`, `gre`, `ip6gre`, `ip6tnl`, `sit`, `tun`, WireGuard). It refuses any other kind, InfiniBand (`ipoib`) for example, rather than read its packets wrongly: the interface is reported unattached (`unattached_interfaces`, `guardAttachFailed`, the `PurelbAddressGuardAttachErrors` alert), and under `failurePolicy: closed` the node stands down.

`excludeInterfaces` exists for the rare link you must leave alone. Every time the configuration is applied the agent logs `guardInterfaceExcluded` for each excluded link, and `kubectl purelb validate` warns about it.

### Hooks

`tcx` (the default) attaches at the head of the link's TC ingress chain. It works on every kind of link and coexists with other eBPF programs. A program attached at the head later runs before it; the agent checks the chain every 30 seconds and after every change it makes, and reports where the guard runs (see [Observability](#observability)).

`xdp` attaches natively in the NIC driver, which drops unwanted packets earlier and more cheaply under a flood. It is used only on Ethernet links whose driver supports it; any link that refuses XDP gets `tcx` instead, and the agent logs `xdpFallbackToTcx`. Two things to know before choosing it:

* attaching XDP makes many drivers reset their queues, briefly interrupting traffic on that NIC. That happens when the guard starts, including on every agent restart;
* a NIC has one XDP attachment point, so the guard and another XDP user (for example a CNI's XDP acceleration) can't share a NIC. The guard never displaces an existing program: it falls back to tcx.

## What it does to VIP traffic

* Traffic to a Service port is untouched, as are replies to clients.
* Fragmented packets: the first fragment carries the ports and is filtered; later fragments pass (the host can't reassemble a datagram without its first fragment). A first fragment too short to contain the ports is dropped.
* IPv6 extension headers are followed (up to eight) to find the ports. AH is treated as a protocol, so it needs `allowedProtocols` in both families.
* The guard decides every frame from what it has read; it never lets a frame through because it couldn't read it. Headers are read wherever the NIC driver put them in the packet. A frame too short for the VLAN tag or IP header it claims, or one with more VLAN tags than QinQ's two, is dropped whatever its destination (in monitor mode, counted as `would_drop` and passed) and counted in `purelb_address_guard_unread_packets_total`. The kernel would discard a truncated frame anyway; the VLAN limit is the one place the guard drops traffic not addressed to a VIP.
* Remote pools aggregated wider than a host route (the default `aggregation` takes the subnet's mask): the kernel also creates an IPv4 subnet-broadcast and an IPv6 subnet-router anycast address on `kube-lb0` for the subnet. Without the guard, UDP sent to them reaches every host socket bound to all addresses. The guard filters them too, with no ports allowed. A pool with host-route aggregation (`/32`, `/128`) gets neither. The allocator never hands those two addresses out as LoadBalancer addresses, whether or not the guard is enabled.
* IPv6 VIPs are added as **deprecated** addresses, so the node never chooses one as the source of its own connections, whose replies the guard would then drop. This applies whether or not the guard is enabled.

## Observability

Metrics, on each node's `:7472/metrics`:

| Metric | Labels | |
|---|---|---|
| `purelb_address_guard_packets_total` | `action` (`pass`, `drop`, `would_drop`), `reason`, `family` | Packets addressed to a guarded VIP. Reasons: `port_allowed`, `proto_allowed`, `icmp`, `fragment`, `port_denied`, `proto_denied`, `icmp_denied`, `malformed`. |
| `purelb_address_guard_vip_packets_total` | `ip`, `action` (`drop`, `would_drop`) | Per VIP. Series appear once non-zero. |
| `purelb_address_guard_unread_packets_total` | `reason` (`truncated`, `vlan_depth`), `action` (`drop`, `would_drop`) | Frames the guard couldn't read, so dropped whatever their destination: shorter than the VLAN tag or IP header they claim, or carrying more than two VLAN tags. |
| `purelb_address_guard_attached` | `interface`, `hook` | 1 for each interface the guard is attached to. |
| `purelb_address_guard_chain_position` | `interface` | Where the guard runs in that interface's tcx ingress chain: 1 is first. Above 1, other programs run before it and can drop or redirect VIP traffic it never sees. tcx only: XDP has one slot. |
| `purelb_address_guard_unattached_interfaces` | `interface` | 1 for each interface the guard should be attached to and isn't, because attaching failed: VIP traffic arriving there is not filtered. The current state; the alert uses it. |
| `purelb_address_guard_attach_errors_total` | `interface` | Attach attempts that left an interface with no hook. |
| `purelb_address_guard_reconcile_deferred_total` | `dump` (`links`, `routes`) | Reconciles put off to the next 30-second check because the kernel kept interrupting a netlink dump (what it was listing was changing, for example pod interfaces coming and going). Nothing changes meanwhile: no interface is detached, no broadcast or anycast address is unguarded, and the node doesn't stand down. |
| `purelb_address_guard_failed_vips` | `effect` (`withheld`, `incomplete`) | Under `failurePolicy: closed`, VIPs whose rules couldn't be written to the guard's maps: `withheld` ones are not announced from this node, `incomplete` ones are filtered by rules that don't match their Services yet. See [Failure behaviour](#failure-behaviour). |
| `purelb_address_guard_standing_down` | | 1 if the node announces no addresses because the guard is fail-closed and isn't working. |
| `purelb_address_guard_unguarded_vips` | `reason` (`not_loaded`, `map_write_failed`, `restart_pending`) | VIPs the configured guard is not filtering on this node. `not_loaded` and `map_write_failed` count only under `failurePolicy: open`: `closed` exposes nothing it can't filter. `restart_pending` counts them, under either policy, while a guard enabled after the agent started waits for a restart. |
| `purelb_address_guard_loaded` | | 1 if the program is loaded. 0 while the guard isn't configured: the program is loaded only when the agent starts with the guard configured. |
| `purelb_address_guard_restart_required` | | 1 if the guard was enabled or disabled after the agent started; restart `lbnodeagent` to apply it fully. |

`purelb_lbnodeagent_selector_state` reports `guardUnavailable` while a node stands down.

`PurelbAddressGuardStandingDown`, `PurelbAddressGuardUnguardedVIPs`, `PurelbAddressGuardAttachErrors`, `PurelbAddressGuardFailedVIPs` and `PurelbAddressGuardRestartRequired` alerts are available: in the Helm chart with `Prometheus.lbnodeagent.prometheusRules.enabled=true` and `addressGuardAlerts=true`, or as `monitoring/prometheusrules-address-guard.yaml`. See [Monitoring](../../operations/monitoring/).

Log events (Info level): `guardLoaded`, `guardNotLoaded`, `guardConfigApplied` (with the mode, hook, failure policy and interfaces), `guardAttached`, `guardDetached`, `guardDetachedExternally`, `guardInterfaceExcluded`, `xdpFallbackToTcx`, `guardAttachFailed`, `guardUnavailable`, `guardMapWriteFailed` (once per address; retries are logged at Debug), `guardConfigWritten`, `guardStandingDown`, `guardStandDownEnded`, `guardRestartRequired`, `guardRestartNotRequired`, `guardNotFirst` (with the IDs of the programs in front), `guardFirstAgain`, `linkSubscribeFailed` / `routeSubscribeFailed` (retried every 30 seconds), and, from the announcer, `addressGuardWithheld`. When another tcx program starts running before the guard, the agent also puts an `AddressGuardNotFirst` Warning Event on the LBNodeAgent; `bpftool prog show id <id>` on the node says what the program is. If something else detaches the guard, the agent's 30-second check notices, attaches it again, and puts an `AddressGuardDetached` Warning Event on the LBNodeAgent.

## Checking it with kubectl purelb

The [kubectl-purelb plugin]({{< relref "/docs/operations/kubectl-plugin" >}}) shows the guard in four places. The [plugin reference]({{< relref "/docs/reference/kubectl-plugin-reference#guard" >}}) has every flag.

### `kubectl purelb guard`: what each node is doing

The live state of the guard on every node, read from the node agents:

```
NODE    MODE     STATE             LOADED  ATTACHED           CHAIN  DROPS  TOP VIP
node-a  enforce  enforcing         yes     eth0/tcx           1      1204   192.0.2.10 (1190)
node-b  enforce  enforcing         yes     eth0/tcx,eth1/tcx  1,2    37     192.0.2.10 (37)
node-c  enforce  RESTART REQUIRED  no      -                  -      0      -
node-b: 1 tcx program(s) run before the guard on eth1 and can drop or redirect VIP traffic it never sees (bpftool net show dev eth1 on the node)
node-c: 2 VIP(s) not filtered (restart_pending)
The guard was enabled or disabled after lbnodeagent started; restart it to apply:
  kubectl -n purelb-system rollout restart daemonset/lbnodeagent
```

* There is one row per node. During a rollout the new agent pod is read, not the terminating one; a node whose agent pod isn't running says so.
* **MODE** is what the node's LBNodeAgent configures. **STATE** is what its agent is actually doing.
* **ATTACHED** lists each interface and its hook. **CHAIN** gives, in the same order, where the guard runs in that interface's tcx chain: `1` is first, `2` means one other program runs before it. It shows `-` for an XDP interface, which has no chain.
* **DROPS** counts packets dropped since the agent started; in monitor mode, would-be drops are shown as `(+N would)`. **TOP VIP** is the address with the most, drops and would-be drops together.
* Below the table, each node's problems: interfaces the guard isn't attached to, VIPs not filtered and why, and VIPs whose rules couldn't be written.

The command exits non-zero only if no node's state could be read at all; with `-o json`, a node it couldn't read has an `error`.

What to do about each state:

State | Meaning | What to do
------|---------|-----------
`enforcing`, `monitoring` | Loaded and attached | Nothing
`off` | Not configured; nothing loaded | Nothing
`RESTART REQUIRED` | Enabled or disabled since the agent started (see [Enabling and disabling](#enabling-and-disabling)) | `kubectl -n purelb-system rollout restart daemonset/lbnodeagent`
`STANDING DOWN` | `failurePolicy: closed` and the guard isn't working: the node announces nothing | `kubectl purelb validate`, and the node's Warning Events
`NOT LOADED` | Configured, but the program couldn't load (`failurePolicy: open` keeps announcing, unfiltered) | `kubectl purelb validate` (kernel, `BPF` capability)
`NOT ATTACHED` | Loaded, but on no interface | The agent's `guardAttachFailed` log lines, and `extraInterfaces` / `excludeInterfaces`
`UNKNOWN` | The agent's metrics couldn't be read; the reason is printed below the table | Usually a missing permission, below

The command reads each agent's metrics through the API server's pod proxy, so it needs `get` on `pods/proxy` in the PureLB namespace. `cmd/kubectl-purelb/rbac-sample.yaml` grants it with a namespaced Role, `purelb:plugin-metrics`, kept apart from the read-only ClusterRole because it is more than read-only: it allows an HTTP GET to any port of any pod in the namespace, and lbnodeagent's pod IP is the node's own address (`hostNetwork`). Without it, the state is `UNKNOWN` and the message names the permission.

### `kubectl purelb status`: one line for the cluster

```
Addr guard:  enforce 4 | monitor 1 (1 restart required)
```

The modes come from the configuration; the note in parentheses comes from the agents, and adds `restart required` and `standing down` counts. Each also raises a warning in the `Overall:` line. `status` reads the agents while some LBNodeAgent configures the guard, and after one has changed, or an agent has reported that it needs a restart, since the oldest agent started: removing the guard leaves its program loaded until the agents restart, and `status` still says so. Otherwise it doesn't read them, so a cluster without the guard pays nothing. `kubectl purelb guard` always reads them.

### `kubectl purelb inspect <namespace>/<service>`: what reaches an address

```
Address guard: enforce on node-a
  192.0.2.10: allows TCP/443, TCP/80, plus allowed ICMP
  2001:db8::10: allows TCP/443, TCP/80, plus allowed ICMP
```

The allowed ports are the union across every Service sharing the address, and any `allowedProtocols` are listed after them. If a client can't reach a port, check it's in this list. In monitor mode the guard drops nothing, and `inspect` says so: the list is what enforce would allow.

### `kubectl purelb validate`: whether the guard can run

```
PASS  LBNodeAgent "purelb-system/default": address guard enforce, failurePolicy closed
```

It checks that the installed LBNodeAgent CRD knows `addressGuard`, that the nodes the guard is configured on run Linux 6.6 or newer, that the lbnodeagent container has the `BPF` capability, and warns about every excluded interface. A problem is a FAIL under `failurePolicy: closed` (the node will stand down) and a WARN under `open`.

## Enabling and disabling

The guard's program and maps are loaded into the kernel only when `lbnodeagent` starts on a node the guard is configured for. A node without the guard runs no guard code at all.

* **Enabled while the agent runs:** nothing changes until the agent restarts. The node keeps announcing, unfiltered as before, under either failure policy. It reports `purelb_address_guard_restart_required` 1 and `unguarded_vips{reason="restart_pending"}`, logs `guardRestartRequired`, and puts an `AddressGuardRestartRequired` Warning Event on the LBNodeAgent. A rolling restart applies it: `kubectl -n purelb-system rollout restart daemonset/lbnodeagent`.
* **Disabled while the agent runs:** the guard detaches from every interface at once, so filtering stops immediately. The program stays loaded until the agent restarts, and the node reports `restart_required` 1 until then.
* **Re-enabled before that restart:** it attaches again at once, since the program is still loaded.

The agents read the configuration for this at startup, so a restart of each agent is what applies it. Don't expect the LBNodeAgent edit alone to do it.

## Failure behaviour

The guard can't do its job on a node when its program can't load (kernel older than 6.6, or no `BPF` capability), when an interface it should guard can't be attached to, and, briefly, when the agent starts and hasn't attached yet. `failurePolicy` decides what the node does then.

Switching the guard back on while the agent runs, after a live disable (the program is still loaded), is different: the node keeps announcing while the guard attaches (milliseconds, during which its VIPs are unfiltered, as they were a moment earlier) and stands down only if attaching fails. Nothing is withdrawn and re-announced when it succeeds. A guard enabled for the first time after the agent started never stands the node down: it waits for a restart (see [Enabling and disabling](#enabling-and-disabling)).

**`closed` (the default): the node stands down.** It announces no addresses until the guard works:

* it stops advertising subnets in its election lease, renewing the lease at once, so no node elects it for a local address and those addresses move to nodes whose guard works;
* it withdraws its remote addresses from `kube-lb0`, so BGP stops advertising them from this node and the router sends that traffic to the other nodes;
* when the guard starts working it announces again.

A local address's *Service* port can keep answering for a little while after the node stands down: until the router's ARP entry for the address ages out, its traffic still reaches the old node, where kube-proxy forwards the Service port as usual. Host ports do not answer, because the address is no longer the node's.

If no node's guard works, the addresses are unreachable: that is what fail-closed means, so make sure the requirements hold before enforcing everywhere. While it stands down the node reports `purelb_address_guard_standing_down` 1 and `selector_state` `guardUnavailable`, logs `guardStandingDown` with the reason, and puts a Warning Event on the LBNodeAgent. Because a starting agent stands down from the moment its configuration arrives until the guard has attached, there is no unguarded moment when the agent starts. A write to the guard's configuration that fails counts as the guard not working, so the node stands down until the retry (every 30 seconds) succeeds.

If one VIP's rules can't be written to the guard's maps (a map is full, or the kernel is out of memory), only that VIP is affected, and every Service sync retries it:

* if the VIP itself can't be added, it can't be filtered, so this node doesn't announce it: `failed_vips{effect="withheld"}`, and the announcer logs `addressGuardWithheld`;
* if one of its ports can't be added, the VIP stays filtered and that Service port is blocked until the retry succeeds: `failed_vips{effect="incomplete"}`.

The maps hold 4096 IPv4 and 4096 IPv6 addresses (VIPs plus the broadcast and anycast addresses above) and 65536 port entries across both families, on every node, since every node guards every VIP.

**`open`: the node keeps announcing, unfiltered, and says so.**

* If the program can't load, nothing is filtered on that node: `purelb_address_guard_loaded` is 0, `unguarded_vips{reason="not_loaded"}` counts the VIPs, and the agent logs `guardUnavailable` and puts a Warning Event on the LBNodeAgent.
* If it can't attach to an interface, that interface is unfiltered: `unattached_interfaces` reports it, and the agent logs and raises an Event.
* If a VIP's rules can't be written, that VIP is left unfiltered rather than having its Service ports dropped, and is retried: `unguarded_vips{reason="map_write_failed"}`.

Neither policy covers an agent **crash**: the program detaches when the agent process dies, and the addresses it held stay on the node until the agent restarts. A graceful restart has no gap.

## Turning it off

Remove `addressGuard` from the LBNodeAgent. The guard detaches from every interface immediately; `purelb_address_guard_attached` disappears from the metrics. The program stays loaded, unattached, until the agents restart; restart them to unload it (`purelb_address_guard_loaded` returns to 0). Nothing is left on the nodes, now or on uninstall.

## Limitations

* An agent crash leaves VIPs unguarded until it restarts, whatever the failure policy. The filter belongs to the lbnodeagent process: when the process exits, for any reason, the kernel detaches the program from every interface and frees it. The VIP addresses don't belong to the process, so they stay on the interfaces. A graceful shutdown (a rolling restart, a `SIGTERM`) withdraws the addresses first and the filter goes last, so nothing is exposed. A crash (a panic, an OOM kill) skips that: the addresses stay without the filter, and host services such as sshd answer on them until the kubelet restarts the agent and it attaches again, longer under `CrashLoopBackOff`. `failurePolicy` doesn't help here, because it is applied by the running agent; once the agent is back, it applies again.
* Only traffic arriving on guarded links is filtered. Traffic from pods or from the node itself to a VIP, and traffic decapsulated from a tunnel, is not: those sources can reach the node's own address anyway.
* Allowing a tunnel protocol (4, 41, 47) in `allowedProtocols` lets encapsulated packets addressed to a VIP be decapsulated by the node, and the packets inside are not filtered.
* VLANs are not distinguished: a Service port is reachable on every VLAN and NIC of the node, as it is without the guard. At most two VLAN tags (QinQ) are supported: a frame with more is dropped (`unread_packets_total{reason="vlan_depth"}`). At the `tcx` hook the two include a tag the kernel has already moved out of the frame; at `xdp`, a NIC that strips the outer tag in hardware hides it, so there a three-tag frame can arrive with two and is filtered normally.
* With kube-proxy in **iptables** mode, VIP:NodePort and `hostPort` access through a VIP stop working: they are exactly the exposure the guard removes. Use the node address for them. IPVS mode is not supported.
* tcx's position at the head of the TC chain holds only at attach time: an eBPF program another component later attaches at the head runs before the guard, and if it ends processing (drops or redirects), the guard never sees the packet. The guard doesn't move itself back in front; it reports the change (`chain_position` above 1, `guardNotFirst`, an `AddressGuardNotFirst` Event, and `kubectl purelb guard`).
* At the `tcx` hook, counters count packets as the kernel sees them after receive offload, so one counted packet can stand for several on the wire. At `xdp` they count frames, so the two hooks' counts aren't comparable.
* With `seg6_enabled=1`, an IPv6 segment-routing header could deliver a packet to a VIP after the guard has inspected it.
* If an IPv4 VIP is promoted to the interface's primary address (`promote_secondaries` and the node address being removed, by a DHCP renewal for example), the node's own traffic is sourced from the VIP and its replies are dropped. Monitor mode shows this as `would_drop` on the VIP before you enforce.
* `loadBalancerSourceRanges` is still enforced by kube-proxy, not by the guard.
