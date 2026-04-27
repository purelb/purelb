---
title: "kubectl Plugin"
description: "Install and use kubectl-purelb for operational visibility into PureLB."
weight: 20
---

The `kubectl-purelb` plugin provides consolidated operational views of PureLB: pool utilization, service announcements, election state, BGP sessions, and configuration validation.

## Installation

Download the binary for your platform from the [latest release](https://github.com/purelb/purelb/releases/latest) and place it in your PATH.

**Linux (amd64):**
```sh
curl -LO https://github.com/purelb/purelb/releases/latest/download/kubectl-purelb-linux-amd64
chmod +x kubectl-purelb-linux-amd64
sudo mv kubectl-purelb-linux-amd64 /usr/local/bin/kubectl-purelb
```

**macOS (Apple Silicon):**
```sh
curl -LO https://github.com/purelb/purelb/releases/latest/download/kubectl-purelb-darwin-arm64
chmod +x kubectl-purelb-darwin-arm64
sudo mv kubectl-purelb-darwin-arm64 /usr/local/bin/kubectl-purelb
```

Verify:
```sh
kubectl purelb version
```

## Commands

Command | Description
--------|------------
`kubectl purelb status` | Cluster-wide health overview: components, pools, election, BGP, services
`kubectl purelb pools` | ServiceGroup pool utilization (total, used, free per range)
`kubectl purelb services` | All PureLB-managed services with announcer info
`kubectl purelb election` | Node Lease status, subnet coverage, health
`kubectl purelb bgp sessions` | BGP neighbor state per node
`kubectl purelb bgp dataplane` | Route pipeline: netlinkImport -> RIB -> advertise -> netlinkExport
`kubectl purelb inspect <ns/svc>` | Deep-dive single service diagnosis
`kubectl purelb validate` | Configuration consistency checks
`kubectl purelb gobgp <args>` | Proxy the gobgp CLI into the k8gobgp sidecar
`kubectl purelb ip <args>` | Proxy the `ip` command into a lbnodeagent pod
`kubectl purelb dashboard` | Live monitoring view in the terminal
`kubectl purelb version` | Show plugin and component versions

All commands support `--namespace`, `--context`, and `--kubeconfig` flags. Most support `--output json` or `--output yaml` for scripting.

See the [kubectl Plugin Reference]({{< relref "/docs/reference/kubectl-plugin-reference" >}}) for complete flag documentation and examples per command.
