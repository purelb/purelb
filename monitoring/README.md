# PureLB Prometheus monitoring

These files allow you to setup basic monitoring in Prometheus and Grafana.

## Grafana dashboard

Currently the grafana dashboard is very simple and has a table for all pools configured with how many IP's are in the pool, free and used.

## Prometheus

For getting the data in Prometheus we require 2 components:

* Service endpoints for the metrics
* Service monitors in Prometheus

## Address guard alerts

`prometheusrules-address-guard.yaml` alerts when:

- a fail-closed node's guard isn't working, so it announces nothing
  (`PurelbAddressGuardStandingDown`, `purelb_address_guard_standing_down`);
- the guard is configured but VIPs are not being filtered
  (`PurelbAddressGuardUnguardedVIPs`, `purelb_address_guard_unguarded_vips`);
- it isn't attached to an interface it should be on
  (`PurelbAddressGuardAttachErrors`, `purelb_address_guard_unattached_interfaces`);
- VIP rules couldn't be written under failurePolicy closed
  (`PurelbAddressGuardFailedVIPs`, `purelb_address_guard_failed_vips`);
- a guard enabled or disabled after the agents started is waiting for an
  `lbnodeagent` restart (`PurelbAddressGuardRestartRequired`,
  `purelb_address_guard_restart_required`).

It needs the Prometheus Operator. Helm users get the same rules with
`Prometheus.lbnodeagent.prometheusRules.enabled=true` and
`Prometheus.lbnodeagent.prometheusRules.addressGuardAlerts=true`.
