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
