# PureLB Prometheus monitoring

These files allow you to setup basic monitoring in Prometheus and Grafana.

## Grafana dashboard

Currently the grafana dashboard is very simple and has a table for all pools configured with how many IP's are in the pool, free and used.

## Prometheus

For getting the data in Prometheus we require 2 components:

* Service endpoints for the metrics
* Service monitors in Prometheus
