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

Shows: component health (allocator, lbnodeagent pods), pool utilization summary, election health, BGP session summary, managed service count, and overall status with warnings.

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

Shows: allocation source, pool type, announcing node/interface, election state, endpoint health, and any detected problems.

## validate

Configuration consistency checks.

```sh
kubectl purelb validate [flags]
```

Flag | Description
-----|------------
`--strict` | Fail on warnings (for CI/CD)

Checks: overlapping pools, unreachable subnets, missing BGP configuration for remote pools, LBNodeAgent consistency.

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
