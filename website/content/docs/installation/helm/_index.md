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
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.16.6
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

> **Note:** These instructions are for upgrading between `purelb.io/v2` releases.
> If you are upgrading from the GitLab **v0.13** release (or any pre-v2 version),
> the CRD API version and namespace changed — follow the
> [v1 to v2 migration guide]({{< relref "/docs/migration/v1-to-v2" >}}) instead.

Helm installs the chart's CRDs on first install but **never upgrades them**.
Apply the new release's CRDs first, then upgrade the chart. Without this the
cluster keeps the old CRD schemas, and the API server silently drops fields the
new release relies on:

```sh
VERSION=v0.17.0   # the release you are upgrading to
kubectl apply --server-side --force-conflicts \
    -f https://github.com/purelb/purelb/releases/download/${VERSION}/install-crds-${VERSION}.yaml
helm repo update
helm upgrade --namespace=purelb-system purelb purelb/purelb --version ${VERSION}
```

Apply `install-crds-${VERSION}.yaml` whatever the chart's values: the chart installs every PureLB CRD, including the k8gobgp and egress ones, so that is the file that updates them all.

Each PureLB CRD carries the release it belongs to (the annotation `purelb.io/crd-version`). The chart checks it: if any installed PureLB CRD is from another release, `helm install` or `helm upgrade` stops before changing anything and tells you to apply the CRDs first. `skipCRDVersionCheck=true` skips the check, for emergencies only. The allocator and the node agents also compare the stamp with their own release, at startup and every 10 minutes, and report a mismatch in `purelb_crd_stale` and a Warning event.

Existing services retain their allocated addresses during the upgrade.
