---
title: "Install with Helm"
description: "Install PureLB using Helm for production deployments."
weight: 30
---

Helm provides full configuration control and is recommended for production deployments.

## Add the Helm Repository

```sh
helm repo add purelb https://purelb.io/charts
helm repo update
```

## Install

```sh
helm install --create-namespace --namespace=purelb-system purelb purelb/purelb
```

### Install Without BGP

```sh
helm install --create-namespace --namespace=purelb-system purelb purelb/purelb \
    --set gobgp.enabled=false
```

### Install via OCI Registry (Helm 3.8+)

```sh
helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.16.3
```

## Key Configuration Values

Value | Default | Description
------|---------|------------
`gobgp.enabled` | `true` | Enable k8gobgp BGP sidecar
`lbnodeagent.localInterface` | `default` | Interface for local address announcement
`lbnodeagent.dummyInterface` | `kube-lb0` | Dummy interface for remote addresses
`leaseConfig.leaseDuration` | `10s` | Election lease duration
`leaseConfig.renewDeadline` | `7s` | Lease renewal deadline
`leaseConfig.retryPeriod` | `2s` | Lease renewal retry interval
`serviceGroup.create` | `false` | Create a default ServiceGroup during install
`defaultAnnouncer` | `PureLB` | LoadBalancer controller name

See the [Helm Values Reference]({{< relref "/docs/reference/helm-values" >}}) for the complete list.

## Overriding Values

Create a YAML file with your overrides:

```yaml
---
allocator:
  tolerations:
  - effect: NoSchedule
    key: node-role.kubernetes.io/control-plane

lbnodeagent:
  garpConfig:
    enabled: true
    count: 3
    interval: 500ms
```

Install with the overrides file:

```sh
helm install --create-namespace --namespace=purelb-system \
    --values=my-values.yaml purelb purelb/purelb
```

## Upgrading

```sh
helm repo update
helm upgrade --namespace=purelb-system purelb purelb/purelb
```

Existing services retain their allocated addresses during the upgrade.
