---
title: "Annotations Reference"
description: "Complete reference for all PureLB annotations."
weight: 20
---

## User-Settable Annotations

These annotations are set by users on Service resources to control PureLB behavior.

Annotation | Value | Description
-----------|-------|------------
`purelb.io/service-group` | ServiceGroup name | Select which ServiceGroup to allocate from. If not set, the `default` ServiceGroup is used.
`purelb.io/addresses` | IP address(es) | Request a specific IP. For dual-stack, comma-separate: `"192.168.1.100,fd00:1::100"`. Must be within the ServiceGroup's pool.
`purelb.io/allow-shared-ip` | sharing key (string) | Enable IP sharing between services with the same key. Services must use different ports. Forces `externalTrafficPolicy: Cluster`.
`purelb.io/allow-local` | `"true"` | Override: allow `externalTrafficPolicy: Local` on local address pools. Not recommended -- local addresses are announced by a single node, so `Local` policy drops traffic when target pods aren't on that node.
`purelb.io/multi-pool` | `"true"` or `"false"` | Override the ServiceGroup's `multiPool` setting for this service.
`purelb.io/re-evaluate` | `"true"` | One-shot trigger to force reprocessing. PureLB deletes this annotation after processing. Useful when subnets change without a config change.

## PureLB-Set Annotations

These annotations are set by PureLB on Service resources. They are informational and should not be modified by users.

Annotation | Value | Description
-----------|-------|------------
`purelb.io/allocated-by` | `"PureLB"` | Marks services managed by PureLB.
`purelb.io/allocated-from` | ServiceGroup name | The ServiceGroup from which addresses were allocated.
`purelb.io/pool-type` | `"local"` or `"remote"` | The type of pool the address came from.
`purelb.io/announcing-IPv4` | `"node,interface,ip"` (see below) | Which node and interface is announcing each IPv4 address.
`purelb.io/announcing-IPv6` | `"node,interface,ip"` (see below) | Which node and interface is announcing each IPv6 address.
`purelb.io/skip-ipv6-dad` | `"true"` | Set when the ServiceGroup has `skipIPv6DAD` enabled.

The `purelb.io/announcing-*` value is a space-separated list of
`node,interface,ip` entries, one per announced address of that family (a
dual-stack or multi-pool Service has several):

```plaintext
purelb.io/announcing-IPv4: node1,enp1s0,192.0.2.10 node2,enp1s0,192.0.2.11
```

Each IP's entry is owned by the node that currently announces it. The
annotation is advisory and eventually consistent: it is set by the announcing
node and, if a node leaves the cluster abruptly, its entry can persist until
another node takes the address or the Service is recreated. For the
authoritative view of who is announcing an address, use `kubectl-purelb
services` (which cross-checks node leases) or the
`purelb_lbnodeagent_announced` metric, rather than reading the annotation
directly. Remote-pool Services carry no announcing annotation; all nodes
announce those addresses on the dummy interface.
