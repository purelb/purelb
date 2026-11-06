---
title: "Monitoring"
description: "Monitor PureLB with Prometheus metrics, ServiceMonitors, and Grafana."
weight: 30
---

PureLB exposes Prometheus metrics from both the Allocator and LBNodeAgent on port 7472 at `/metrics`.

## Metrics

All metrics are documented in the [Metrics Reference]({{< relref "/docs/reference/metrics" >}}). Key metrics to watch:

- **Pool exhaustion:** `purelb_address_pool_addresses_in_use` vs `purelb_address_pool_size`
- **Election health:** `purelb_election_lease_healthy`, `purelb_election_member_count`
- **Egress:** `purelb_egress_policy_conditions_false`, `purelb_egress_node_conditions_false`, `purelb_egress_slice_utilization_ratio`, `purelb_egress_datapath_packets_total{action="drop"}`
- **Node agent activity:** `purelb_lbnodeagent_garp_errors_total`, `purelb_lbnodeagent_address_additions_total`

## Setting Up Prometheus

### ServiceMonitor (Prometheus Operator)

If using the Prometheus Operator, enable ServiceMonitors via Helm:

```yaml
Prometheus:
  allocator:
    Metrics:
      enabled: true
    serviceMonitor:
      enabled: true
  lbnodeagent:
    Metrics:
      enabled: true
    serviceMonitor:
      enabled: true
```

Or apply the ServiceMonitor manifests directly from the repository's `monitoring/service-monitors.yaml`.

### PrometheusRules

Example alert for pool exhaustion:

```yaml
Prometheus:
  allocator:
    prometheusRules:
      enabled: true
      rules:
      - alert: PureLBPoolNearlyExhausted
        expr: purelb_address_pool_addresses_in_use / purelb_address_pool_size > 0.9
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Pool {{ $labels.pool }} is over 90% utilized"
```

The address guard ships its own alerts:

* `PurelbAddressGuardStandingDown`: a fail-closed node's guard isn't working, so the node announces no addresses (2 minutes).
* `PurelbAddressGuardUnguardedVIPs`: under `failurePolicy: open`, some VIPs on a node are not being filtered because the program isn't running or their rules couldn't be written (5 minutes). A guard enabled after `lbnodeagent` started is `PurelbAddressGuardRestartRequired` instead.
* `PurelbAddressGuardAttachErrors`: the guard isn't attached to an interface it should be on (5 minutes).
* `PurelbAddressGuardFailedVIPs`: under `failurePolicy: closed`, some VIPs' rules couldn't be written, so they aren't announced from that node or a Service port is blocked (5 minutes).
* `PurelbAddressGuardRestartRequired`: the guard was enabled or disabled after `lbnodeagent` started, and the agents haven't been restarted for 15 minutes: an enabled guard filters nothing until they are.

Turn them on alongside your own rules:

```yaml
Prometheus:
  lbnodeagent:
    prometheusRules:
      enabled: true
      addressGuardAlerts: true
```

Without Helm, apply `monitoring/prometheusrules-address-guard.yaml` from the repository.

## Egress Alerts

Turn on PureLB's egress alerts alongside your own rules. They need both the allocator's and the agents' metrics scraped:

```yaml
Prometheus:
  lbnodeagent:
    prometheusRules:
      enabled: true
      egressAlerts: true
```

Without Helm, apply `monitoring/prometheusrules-egress.yaml` from the repository. Each alert links to its runbook here.

### PurelbEgressNoAnnouncer

Severity: critical. A policy's address has no node with a live Lease announcing it, so traffic through it doesn't reach the network.

`kubectl get egress -A` shows `Announced=False NoAnnouncer`. Check that `announcerNodeSelector` selects egress-capable nodes with live Leases (`kubectl purelb election`), and for a routed pool that those nodes peer. `kubectl purelb egress dump` shows each node's conditions.

### PurelbEgressSplitBrain

Severity: critical. Two nodes report announcing a SingleAnnouncer address, for at least two lease durations, so replies may reach the wrong node.

Look at each announcer's Lease and connectivity: `kubectl purelb election`. A partitioned node keeps its address until it sees the Lease. Fix the partition, or stop the agent on the stale node.

### PurelbEgressNATInsertFailed

Severity: critical. On the eBPF stage, new flows from selected pods are dropped because their slice has no free port or the NAT table (131,072 entries) is full.

An answered UDP flow holds its port for 120 s, so a 4,096-port slice fills above about 30 new flows a second. Raise `ports.sliceSize`, or use more addresses.

### PurelbEgressSliceExhaustion

Severity: critical. An address has 80% or more of its usable slices active or draining. Once none are free, new pod nodes can't get one.

Check `kubectl get egress -A -o yaml` for `capacity`. Use a smaller `sliceSize`, add an address, or wait for draining slices (5 minutes at most).

### PurelbEgressChainMissing

Severity: critical. The egress CNI plugin isn't in a node's CNI chain, so new pods there start without egress's check.

`kubectl purelb egress dump` gives the cause. `ChainMissing`: is `purelb-cni` running there? `CNIChainMissing`: the CNI owns its config (Cilium: add the plugin through `cni.configMap`, see [Egress]({{< relref "/docs/configuration/egress" >}})), or its primary file is a single-plugin `.conf`.

### PurelbEgressCNIAddFailing

Severity: critical. Pods can't start on a node because the plugin's ADD fails. kubelet retries.

The agent logs each error (`op=egressCNI`). A FailClosed pod's ADD fails on purpose until its stage and slot are in place, so look for a node whose agent is down or `DatapathReady=False`.

### PurelbEgressCNIAddSlow

Severity: critical. The p99 wait for a selected pod's slice and classifier is above the 25 s hold. Pods then start without their slice, or fail, as their `failurePolicy` says.

Usually the allocator or API server is overloaded by a large rollout. Check the allocator's logs and `purelb_egress_lagging_nodes`.

### PurelbEgressDatapathFailed

Severity: critical. A node couldn't load or program the egress datapath, and keeps retrying.

`kubectl purelb egress dump --node <node>` shows the condition message; the agent's log (`op=egressDatapath`) has the error. Check the kernel with `kubectl purelb validate --live`.

### PurelbEgressForgedDrops

Severity: warning. A node is dropping packets as `forged_*` or `not_announcer`.

`forged_tunnel`: something other than a node sends to the tunnel port. `forged_inner`: a node sends outside its slice. `not_announcer`: the network routes replies to a node that doesn't hold the address, so check your routes and, for a local pool, ARP or NDP caches.

### PurelbEgressGenerationLag

Severity: warning. Nodes were evicted from a policy's gating because they didn't ack its slice table. They can serve a stale one until they catch up.

Look for an overloaded or partitioned agent. It rejoins by itself once it reports the address again.

### PurelbEgressDADFailed

Severity: warning. Another host on the link has an egress address, so it isn't announced from that node. The node retries with backoff.

Find the other host: `ip -6 neigh` on the link. Take the address out of the pool, or remove it from that host.

### PurelbEgressForwardBlocked

Severity: warning. Selected pods' new flows are marked but none reaches its SNAT: a forward filter (Docker, firewalld), `rp_filter`, or a NetworkPolicy denying every selected pod.

PureLB never edits other firewalls. Allow forwarded traffic from pods, or fix the policy.

### PurelbEgressTunnelPeerDown

Severity: warning. Tunnel peers aren't answering a node's probes.

`kubectl purelb egress dump --node <node>` names them in `TunnelPeersReachable`. Open the tunnel port (`tunnelPort`, UDP) between nodes. If no peer answers, the condition is `ForwardPath=False TunnelPortBlocked`.

### PurelbEgressUnderlayUnencrypted

Severity: warning. The node encrypts node-to-node traffic (WireGuard, IPsec), but the egress tunnel doesn't pass through it, so tunnelled pod traffic crosses the underlay unencrypted.

Accept it knowingly, or keep selected pods on nodes holding their address so their traffic goes direct.

### PurelbEgressCNISnapshotOld

Severity: warning. A node's agent hasn't written the CNI snapshot for over 30 minutes. At its age bound (1 h by default) the plugin passes pods through unchecked.

The agent is down or can't write `/var/lib/purelb`. Check the agent's pod and log.

### PurelbEgressOrphanPins

Severity: warning. Objects are still pinned after egress was turned off and removed from a node.

`kubectl purelb egress cleanup --node <node>` removes them.

## Useful PromQL Queries

**Pool utilization percentage:**
```
purelb_address_pool_addresses_in_use / purelb_address_pool_size * 100
```

**Unhealthy nodes:**
```
purelb_election_lease_healthy == 0
```

**GARP error rate:**
```
rate(purelb_lbnodeagent_garp_errors_total[5m])
```

**Address churn rate:**
```
rate(purelb_lbnodeagent_address_additions_total[5m]) + rate(purelb_lbnodeagent_address_withdrawals_total[5m])
```

## Grafana Dashboard

A pre-built Grafana dashboard is available at `monitoring/dashboard.json` in the PureLB repository. It shows pool utilization tables and node agent health.

## Terminal Monitoring

For quick monitoring without Prometheus:

```sh
kubectl purelb dashboard
```

This shows a live terminal view of pool status, election health, and BGP sessions.
