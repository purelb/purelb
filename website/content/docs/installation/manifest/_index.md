---
title: "Install with Manifests"
description: "Install PureLB using kubectl apply."
weight: 20
---

The quickest way to install PureLB is with kubectl. Installation is a two-step process: apply the CRDs first, then the main manifest.

## With BGP Support (Default)

```sh
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-v0.16.3.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-v0.16.3.yaml
```

This installs PureLB with the k8gobgp sidecar for BGP route advertisement. After installing, create a [BGPConfiguration]({{< relref "/docs/configuration/bgp" >}}) CR to configure BGP peering.

## Without BGP Support

```sh
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-nobgp-v0.16.3.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-nobgp-v0.16.3.yaml
```

Use this variant if you only need local addresses (same subnet as your nodes) and do not need BGP routing.

> [!NOTE]
> CRDs must be applied before the main manifest because the install manifest includes a default `LBNodeAgent` custom resource that requires its CRD to be registered in the API server.

## Verify Installation

Check that the allocator and lbnodeagent pods are running:

```sh
kubectl get pods -n purelb-system -o wide
```

With BGP support, each lbnodeagent pod shows `2/2` READY (the second container is k8gobgp):

```plaintext
NAME                         READY   STATUS    RESTARTS   AGE   IP               NODE
allocator-6b8f7d4c5-x9k2m   1/1     Running   0          60s   10.244.0.15      node1
lbnodeagent-4h7nq            2/2     Running   0          60s   192.168.1.101    node1
lbnodeagent-8r2kp            2/2     Running   0          60s   192.168.1.102    node2
lbnodeagent-m5j6w            2/2     Running   0          60s   192.168.1.103    node3
```

Without BGP, lbnodeagent pods show `1/1` READY.

## Installed Components

The manifest creates:

1. **purelb-system namespace** -- All PureLB components run here.
2. **CRDs** -- `ServiceGroup` and `LBNodeAgent` (plus `BGPConfiguration` and `BGPNodeStatus` if BGP is enabled).
3. **Allocator Deployment** -- A single replica that watches Services and allocates IPs.
4. **LBNodeAgent DaemonSet** -- One pod per node that configures Linux networking. With BGP, includes the k8gobgp sidecar.
5. **Default LBNodeAgent CR** -- A minimal configuration that works for most environments.

## Upgrading

To upgrade, apply the new version's manifests:

```sh
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-v0.16.3.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-v0.16.3.yaml
```

Existing services retain their allocated addresses during the upgrade.
