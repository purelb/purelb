---
title: "kubectl Plugin Reference"
description: "Complete reference for all kubectl-purelb subcommands and flags."
weight: 50
---

All commands support these global flags:

Flag | Description
-----|------------
`--kubeconfig` | Path to kubeconfig file
`--context` | Kubernetes context to use
`--namespace` / `-n` | Namespace (default: `purelb-system` for most commands)
`--output` / `-o` | Output format: `json`, `yaml`, or default table

## status

Cluster-wide health overview.

```sh
kubectl purelb status
```

Shows: component health (allocator, lbnodeagent pods), pool utilization summary, election health, BGP session summary, managed service count, the [address guard]({{< relref "/docs/configuration/address-guard" >}}) mode across nodes (for example `enforce 4 | monitor 1`, or `off`) with any nodes that need an `lbnodeagent` restart or have stood down (read live from the agents, as `guard` does), and overall status with warnings.

## pools

ServiceGroup pool utilization.

```sh
kubectl purelb pools [flags]
```

Flag | Description
-----|------------
`--service-group` | Filter to a specific ServiceGroup
`--show-services` | Show which services are using each pool

## services

All PureLB-managed services.

```sh
kubectl purelb services [flags]
```

Flag | Description
-----|------------
`--all-namespaces` / `-A` | Show services from all namespaces
`--pool` | Filter by ServiceGroup name
`--node` | Filter by announcing node
`--ip` | Filter by allocated IP
`--problems` | Show only services with detected issues

## election

Node Lease status and subnet coverage.

```sh
kubectl purelb election [flags]
```

Flag | Description
-----|------------
`--check` | Run health checks and report problems
`--node` | Show details for a specific node
`--simulate-drain` | Show what would happen if a node were drained

## bgp sessions

BGP neighbor state per node.

```sh
kubectl purelb bgp sessions [flags]
```

Flag | Description
-----|------------
`--check` | Run health checks and report problems
`--node` | Filter to a specific node

## bgp dataplane

Route pipeline health: netlinkImport -> RIB -> advertise -> netlinkExport.

```sh
kubectl purelb bgp dataplane [flags]
```

Flag | Description
-----|------------
`--check` | Run health checks and report problems
`--import-only` | Show only import pipeline
`--export-only` | Show only export pipeline

## inspect

Deep-dive diagnosis of a single service.

```sh
kubectl purelb inspect <namespace>/<service>
```

Shows: allocation source, pool type, announcing node/interface, election state, endpoint health, what the address guard lets through to each address (the Service ports, combined across Services sharing it, plus allowed ICMP) and its mode where the address is filtered, and any detected problems. The announcing node is cross-checked against node leases, so an entry left behind by a departed node is flagged as unhealthy rather than reported as the current announcer.

## guard

The address guard on each node, read live from the node agents.

```sh
kubectl purelb guard [flags]
```

Flag | Description
-----|------------
`--node` | Only this node
`-o`, `--output` | `json` or `yaml`

For each node: the configured mode (from the LBNodeAgent), the state, whether the program is loaded, the interfaces and hooks it is attached to, where it runs in each tcx chain (CHAIN: one number per attached interface, in the ATTACHED order; `1` means it runs first, `2` that one program runs before it; `-` for an XDP link, which has no chain; `?` when the agent doesn't report it), packets dropped (and, in monitor mode, would-be drops), and the VIP with the most, counting both. Packet counts are cumulative since that agent started. Below the table: interfaces the guard should be on and isn't, VIPs not filtered and why, and VIPs whose rules couldn't be written. One row per node: during a rollout the running agent pod is read, not the terminating one.

Agents are read 32 at a time, each with its own 10-second timeout. The command exits non-zero only when no node's state could be read.

State | Meaning
------|--------
`enforcing`, `monitoring` | Loaded and attached
`off` | Not configured, nothing loaded
`RESTART REQUIRED` | Enabled or disabled after the agent started; restart `lbnodeagent` to apply it
`STANDING DOWN` | Configured with `failurePolicy: closed` and not working: the node announces nothing
`NOT LOADED` | Configured, but the program couldn't load (kernel or `BPF` capability)
`NOT ATTACHED` | Loaded, but attached to no interface
`UNKNOWN` | The agent's metrics couldn't be read; the reason is printed below the table

This state exists only in the agents' metrics, which the plugin reads through the API server's pod proxy: it needs `get` on `pods/proxy` in the PureLB namespace, which `cmd/kubectl-purelb/rbac-sample.yaml` grants with a separate namespaced Role (`purelb:plugin-metrics`). That permission is wider than reading metrics: it lets its holder send an HTTP GET to any port of any pod in the namespace, and because lbnodeagent runs with `hostNetwork`, that means any HTTP service on a node's address the API server can reach. Bind it only to users who should have that. Without it, the command says so and shows the state as unknown. `status` reads the same state to flag nodes that need a restart or have stood down: while some LBNodeAgent configures the guard, and after a change that may have removed it (an LBNodeAgent edited, or an `AddressGuardRestartRequired` Event, since the oldest agent started).

## egress trace

What egress does with a pod's flow.

```sh
kubectl purelb egress trace <namespace/pod> --dst <address[:port][/protocol]> [flags]
```

Flag | Description
-----|------------
`--dst` | Destination. IPv6 with a port: `[2001:db8::1]:53/udp`. The protocol defaults to TCP
`--live` | Also read the pod's node's and the holders' datapaths
`-o`, `--output` | `json` or `yaml`

The policies selecting the pod, by rank, and which cover the destination; the winner and its `failurePolicy`; the address and ports the pod's node has; the pod stage; the nodes holding the address, and whether the flow goes `direct` or through the `tunnel`. It's computed from API objects, without knowing the cluster's networks, so a destination inside the cluster reads as covered and doesn't use egress.

`--live` needs `create` on `pods/exec` in the PureLB namespace: the `purelb:plugin-egress-live` Role in `rbac-sample.yaml`. That runs commands as the agent, with its capabilities.

## egress dump

Each node's egress conditions and datapath.

```sh
kubectl purelb egress dump [flags]
```

Flag | Description
-----|------------
`--node` | Only this node
`-o`, `--output` | `json` or `yaml`

The node's conditions that are False and its faults, then what the kernel has: pod stage, NAT flow count, pod devices, each address with its slice owners and traffic, slots, and with `-o json` packet counts by reason. Needs the same `pods/exec` access as `trace --live`.

## egress cleanup

Break-glass: remove egress from nodes whose agent can't.

```sh
kubectl purelb egress cleanup (--node <node> | --all) [--force]
```

Runs a privileged pod on each node that removes the CNI chain entry and plugin, programs, pins, nft table, routing rules and tunnel, and the plugin's files. It resets egress's flows. It refuses while an LBNodeAgent enables egress, since the agent would put it all back, unless given `--force`. Needs the `purelb:plugin-egress-admin` Role in `rbac-sample.yaml`.

## egress release-finalizers

Break-glass: free policies stuck deleting because the allocator is gone.

```sh
kubectl purelb egress release-finalizers (<namespace/name> | <name> | --all)
```

Removes `purelb.io/egress` from an Egress (`<namespace/name>`) or ClusterEgress (`<name>`) and its EgressEndpoints. Quarantine and any external IPAM's release are skipped. Needs the `purelb:plugin-egress-admin` roles in `rbac-sample.yaml`.

## validate

Configuration consistency checks.

```sh
kubectl purelb validate [flags]
```

Flag | Description
-----|------------
`--strict` | Fail on warnings (for CI/CD)
`--live` | Also check each node's egress settings: kernel config, bpffs, forwarding, `arp_announce`, chain order. Runs `lbnodeagent diag` through `kubectl exec`

Checks: overlapping pools, unreachable subnets, missing BGP configuration for remote pools, LBNodeAgent consistency, and for the address guard: an installed LBNodeAgent CRD that would silently drop `addressGuard`, nodes whose kernel is too old to run it, an lbnodeagent container without the `BPF` capability, and excluded interfaces.

The capability and CRD checks read the lbnodeagent DaemonSet and the LBNodeAgent CRD. The sample ClusterRole in `cmd/kubectl-purelb/rbac-sample.yaml` grants that read access; without it these checks report "unable to check" instead of failing.

## gobgp

Proxy the gobgp CLI into the k8gobgp sidecar.

```sh
kubectl purelb gobgp <gobgp-args>
```

Examples:
```sh
kubectl purelb gobgp neighbor
kubectl purelb gobgp global rib -a ipv4
kubectl purelb gobgp global rib -a ipv6
```

## ip

Proxy the `ip` command into a lbnodeagent pod.

```sh
kubectl purelb ip <ip-args>
```

Examples:
```sh
kubectl purelb ip addr show
kubectl purelb ip addr show dev kube-lb0
kubectl purelb ip route show
```

## dashboard

Live terminal monitoring view.

```sh
kubectl purelb dashboard
```

Shows a consolidated live view of pool status, election health, and BGP sessions.

## version

Show plugin and component versions.

```sh
kubectl purelb version
```
