# PureLB Prometheus monitoring

These files allow you to setup basic monitoring in Prometheus and Grafana.

## Grafana dashboard

Currently the grafana dashboard is very simple and has a table for all pools configured with how many IP's are in the pool, free and used.

## Prometheus

For getting the data in Prometheus we require 2 components:

* Service endpoints for the metrics
* Service monitors in Prometheus

## Address guard alerts

`prometheusrules-address-guard.yaml` alerts when the address guard is
configured but VIPs are not being filtered (`purelb_address_guard_unguarded_vips`)
or it could not attach to an interface (`purelb_address_guard_attach_errors_total`),
and when a guard enabled or disabled after the agents started is waiting for
an `lbnodeagent` restart (`purelb_address_guard_restart_required`).
It needs the Prometheus Operator. Helm users get the same rules with
`Prometheus.lbnodeagent.prometheusRules.addressGuardAlerts=true`.
