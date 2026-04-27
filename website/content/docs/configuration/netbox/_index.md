---
title: "Netbox IPAM Integration"
description: "Configure PureLB to allocate addresses from an external Netbox IPAM system."
weight: 40
---

> [!WARNING]
> Netbox IPAM integration is currently **unsupported**. The code exists but has not been tested against recent Netbox versions and may not work correctly. Use at your own risk. If you need external IPAM integration, please open a [GitHub issue](https://github.com/purelb/purelb/issues) to discuss your requirements.

PureLB can request IP addresses from [Netbox](https://github.com/netbox-community/netbox), a popular open source IP Address Management system. Instead of managing address pools locally, PureLB's Allocator requests addresses from Netbox one at a time, and Netbox tracks allocation across your entire infrastructure.

## Netbox Setup

Before configuring PureLB, ensure your Netbox instance has:

1. **A tenant** configured for PureLB to allocate from.
2. **Prefixes** defined for the address ranges PureLB will use.
3. **An API token** with the following permissions:
   - `ipam.view_ipaddress`
   - `ipam.change_ipaddress`

## Create the API Token Secret

PureLB reads the Netbox API token from a Kubernetes Secret:

```sh
kubectl create secret generic netbox-client \
    --namespace=purelb-system \
    --from-literal=user-token=YOUR_NETBOX_API_TOKEN
```

## Configure the ServiceGroup

Create a ServiceGroup with the `netbox` spec:

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: netbox-pool
  namespace: purelb-system
spec:
  netbox:
    url: https://netbox.example.com/api
    tenant: kubernetes-cluster
```

Field | Type | Required | Description
------|------|----------|------------
`url` | string | Yes | Base URL of the Netbox API
`tenant` | string | Yes | Netbox tenant name for IP allocation
`aggregation` | string | No | Override address mask (e.g., `/32`). Default uses the prefix mask from Netbox.

## How It Works

1. A Service requests an address from the `netbox-pool` ServiceGroup.
2. The Allocator queries the Netbox API for the next available IP address in the tenant's prefixes.
3. Netbox marks the address as allocated and returns it.
4. The Allocator writes the address to the Service status.
5. When the Service is deleted, the Allocator releases the address back to Netbox.

PureLB applies the address to interfaces in the same way as locally managed pools -- the address type (local vs remote) is determined by whether the address is on the same subnet as the node.

## IPv4 and IPv6

Netbox supports both IPv4 and IPv6 prefixes. PureLB requests addresses from the appropriate address family based on the Service's `ipFamilyPolicy` and `ipFamilies` settings.
