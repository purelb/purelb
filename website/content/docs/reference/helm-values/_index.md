---
title: "Helm Values Reference"
description: "Complete reference for all PureLB Helm chart configuration values."
weight: 40
---

## Image

Value | Type | Default | Description
------|------|---------|------------
`image.repository` | string | (set by chart) | Container image repository
`image.pullPolicy` | string | `IfNotPresent` | Image pull policy
`image.tag` | string | (chart appVersion) | Image tag override

## General

Value | Type | Default | Description
------|------|---------|------------
`nameOverride` | string | `""` | Override chart name
`fullnameOverride` | string | `""` | Override full release name
`defaultAnnouncer` | string | `"PureLB"` | When `"PureLB"`, handle services without `loadBalancerClass`. Set to anything else to require explicit `loadBalancerClass: purelb.io/purelb`.
`priorityClassName` | string | `""` | PriorityClass for allocator and lbnodeagent pods
`memberlistSecretKey` | string | (deprecated) | **Deprecated.** No longer used. Kept for backward compatibility.

## Lease Configuration

Value | Type | Default | Description
------|------|---------|------------
`leaseConfig.leaseDuration` | string | `"10s"` | How long a Lease is valid before expiry
`leaseConfig.renewDeadline` | string | `"7s"` | How long to retry renewals before giving up
`leaseConfig.retryPeriod` | string | `"2s"` | Interval between renewal attempts

## ServiceGroup

Value | Type | Default | Description
------|------|---------|------------
`serviceGroup.name` | string | `"default"` | Name of the ServiceGroup to create
`serviceGroup.create` | bool | `false` | Create a default ServiceGroup during install
`serviceGroup.spec` | object | `{}` | ServiceGroup spec (e.g., `local.v4pool.subnet`, `local.v4pool.pool`, `local.v4pool.aggregation`)

## LBNodeAgent

Value | Type | Default | Description
------|------|---------|------------
`lbnodeagent.localInterface` | string | `"default"` | Interface for local address announcement
`lbnodeagent.dummyInterface` | string | `"kube-lb0"` | Dummy interface for remote addresses
`lbnodeagent.garpConfig` | object | (not set) | GARP configuration: `enabled`, `count`, `interval`, `initialDelay`
`lbnodeagent.containerSecurityContext` | object | (see below) | Container security context
`lbnodeagent.tolerations` | []object | `[]` | Pod tolerations
`lbnodeagent.nodeSelector` | object | | Node selector labels

Default lbnodeagent security context: `runAsUser: 0`, `capabilities: [NET_ADMIN, NET_RAW]`, `readOnlyRootFilesystem: false`.

## Allocator

Value | Type | Default | Description
------|------|---------|------------
`allocator.containerSecurityContext` | object | (see below) | Container security context
`allocator.tolerations` | []object | `[]` | Pod tolerations
`allocator.securityContext` | object | (see below) | Pod security context

Default allocator security context: `runAsNonRoot: true`, `runAsUser: 65534`, `readOnlyRootFilesystem: true`, `capabilities: drop all`.

## k8gobgp Sidecar

Value | Type | Default | Description
------|------|---------|------------
`gobgp.enabled` | bool | `true` | Enable k8gobgp BGP sidecar in the lbnodeagent DaemonSet
`gobgp.image.repository` | string | `ghcr.io/purelb/k8gobgp` | k8gobgp container image
`gobgp.image.tag` | string | `"0.2.2"` | k8gobgp image tag
`gobgp.image.pullPolicy` | string | `IfNotPresent` | Image pull policy
`gobgp.containerSecurityContext` | object | (see below) | Container security context
`gobgp.resources.requests.cpu` | string | `250m` | CPU request
`gobgp.resources.requests.memory` | string | `128Mi` | Memory request
`gobgp.resources.limits.cpu` | string | `1000m` | CPU limit
`gobgp.resources.limits.memory` | string | `512Mi` | Memory limit

Default k8gobgp security context: `capabilities: [NET_ADMIN, NET_BIND_SERVICE, NET_RAW]`, `readOnlyRootFilesystem: true`.

## Prometheus Monitoring

Value | Type | Default | Description
------|------|---------|------------
`Prometheus.allocator.Metrics.enabled` | bool | `false` | Create metrics Service for allocator
`Prometheus.allocator.serviceMonitor.enabled` | bool | `false` | Create ServiceMonitor for allocator
`Prometheus.allocator.serviceMonitor.extraLabels` | object | `{}` | Additional labels on ServiceMonitor
`Prometheus.allocator.prometheusRules.enabled` | bool | `false` | Create PrometheusRules for allocator
`Prometheus.allocator.prometheusRules.namespace` | string | `""` | Namespace for PrometheusRules
`Prometheus.allocator.prometheusRules.rules` | []object | `[]` | Alert rules
`Prometheus.lbnodeagent.Metrics.enabled` | bool | `false` | Create metrics Service for lbnodeagent
`Prometheus.lbnodeagent.serviceMonitor.enabled` | bool | `false` | Create ServiceMonitor for lbnodeagent
`Prometheus.lbnodeagent.serviceMonitor.extraLabels` | object | `{}` | Additional labels on ServiceMonitor
`Prometheus.lbnodeagent.prometheusRules.enabled` | bool | `false` | Create PrometheusRules for lbnodeagent
`Prometheus.lbnodeagent.prometheusRules.rules` | []object | `[]` | Alert rules

## Extra Objects

Value | Type | Default | Description
------|------|---------|------------
`extraObjects` | []string | `[]` | List of arbitrary Kubernetes manifests to create (templated)
