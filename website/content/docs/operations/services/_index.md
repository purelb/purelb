---
title: "Managing Services"
description: "Create and manage LoadBalancer Services with PureLB annotations."
weight: 10
---

## Creating a LoadBalancer Service

Set `type: LoadBalancer` in your Service spec. PureLB watches for these services and allocates addresses automatically.

### Basic IPv4 Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
spec:
  type: LoadBalancer
  selector:
    app: my-app
  ports:
  - port: 80
    targetPort: 8080
```

### Dual-Stack Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  ipFamilyPolicy: PreferDualStack
  ipFamilies:
  - IPv4
  - IPv6
  selector:
    app: my-app
  ports:
  - port: 80
    targetPort: 8080
```

`PreferDualStack` works on both single-stack and dual-stack clusters.

## Annotations

### Selecting a ServiceGroup

```yaml
annotations:
  purelb.io/service-group: routed
```

If omitted, PureLB uses the ServiceGroup named `default`.

### Requesting a Specific Address

```yaml
annotations:
  purelb.io/addresses: "192.168.1.100"
```

For dual-stack, comma-separate the addresses:

```yaml
annotations:
  purelb.io/addresses: "192.168.1.100,fd00:1::100"
```

The requested address must be within the ServiceGroup's pool range.

### Sharing an IP Address

Multiple services can share a single IP if they use different ports. Set the same sharing key on each service:

```yaml
# Service A
annotations:
  purelb.io/allow-shared-ip: "my-shared-vip"
---
# Service B
annotations:
  purelb.io/allow-shared-ip: "my-shared-vip"
```

ExternalTrafficPolicy is forced to `Cluster` for shared addresses.

### ExternalTrafficPolicy Local

`externalTrafficPolicy: Local` is supported for **remote address pools only**. This enables Direct Server Return (DSR): traffic arrives at the node and is delivered directly to the pod without traversing the CNI or being rewritten by kube-proxy. The original client source IP and destination address are preserved end-to-end. PureLB only adds the address to nodes running target pods, so BGP only advertises routes from those nodes.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  annotations:
    purelb.io/service-group: routed
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  selector:
    app: my-app
  ports:
  - port: 80
    targetPort: 8080
```

For local address pools, PureLB always uses `externalTrafficPolicy: Cluster`. If a service with a local pool is set to `Local`, PureLB resets it to `Cluster` automatically.

### Multi-Pool Override

Override the ServiceGroup's `multiPool` setting for a specific service:

```yaml
annotations:
  purelb.io/multi-pool: "true"
```

### Force Re-Evaluation

PureLB evaluates a service once at creation time. If the network environment changes after allocation -- for example, nodes are added to new subnets, ServiceGroup pools are expanded, or multi-pool ranges are updated -- existing services won't automatically pick up the changes.

The `re-evaluate` annotation triggers PureLB to reprocess the service as if it were newly created:

```yaml
annotations:
  purelb.io/re-evaluate: "true"
```

This is a one-shot trigger -- PureLB deletes the annotation after processing. The service retains its existing addresses but may gain additional addresses (e.g., from newly-available multi-pool ranges) or be validated against updated pool configuration.

**Common use cases:**
- A new subnet was added to a `multiPool` ServiceGroup and existing services need addresses on the new subnet.
- A pool was expanded and you want to verify services are still correctly allocated.
- Node topology changed and you want election re-evaluation.

## Kubernetes Service Fields

Field | Description
------|------------
`spec.type` | Must be `LoadBalancer`
`spec.externalTrafficPolicy` | `Cluster` (default) or `Local` (remote pools only)
`spec.allocateLoadBalancerNodePorts` | Set to `false` to skip NodePort allocation (recommended)
`spec.loadBalancerClass` | Set to `purelb.io/purelb` to explicitly target PureLB (useful with multiple LB controllers)
`spec.ipFamilyPolicy` | `SingleStack`, `PreferDualStack`, or `RequireDualStack`
`spec.ipFamilies` | `[IPv4]`, `[IPv6]`, or `[IPv4, IPv6]`

## Reading Service Status

PureLB adds informational annotations to allocated services:

```sh
kubectl describe svc my-app
```

```plaintext
Annotations:  purelb.io/allocated-by: PureLB
              purelb.io/allocated-from: default
              purelb.io/pool-type: local
              purelb.io/announcing-IPv4: node1,enp1s0
              purelb.io/service-group: default
```

Annotation | Description
-----------|------------
`purelb.io/allocated-by` | Set to `PureLB` on services PureLB manages
`purelb.io/allocated-from` | ServiceGroup that provided the address
`purelb.io/pool-type` | `local` or `remote`
`purelb.io/announcing-IPv4` | Node and interface announcing the IPv4 address
`purelb.io/announcing-IPv6` | Node and interface announcing the IPv6 address

For detailed inspection, use the [kubectl plugin]({{< relref "/docs/operations/kubectl-plugin" >}}):

```sh
kubectl purelb inspect default/my-app
```

## Complete Annotation Reference

See the [Annotations Reference]({{< relref "/docs/reference/annotations" >}}) for the full list.
