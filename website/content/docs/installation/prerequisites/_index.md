---
title: "Prerequisites"
description: "Cluster requirements and preparation before installing PureLB."
weight: 10
---

## Kubernetes Cluster

PureLB requires a Kubernetes cluster with:

- A working [Container Network Interface](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/network-plugins/) (CNI). The CNI must be operational before PureLB is installed.
- Linux nodes. PureLB uses the Linux netlink API to configure interfaces and routes.

## ARP Behavior

We recommend changing the Linux kernel's ARP behavior from its default. By default, Linux answers ARP requests for addresses on any interface, regardless of which interface received the request. This can cause problems when PureLB adds LoadBalancer addresses to interfaces.

Configure the kernel to only respond to ARP requests for addresses on the receiving interface:

```sh
cat <<EOF | sudo tee /etc/sysctl.d/k8s_arp.conf
net.ipv4.conf.all.arp_ignore=1
net.ipv4.conf.all.arp_announce=2
EOF
sudo sysctl --system
```

> [!WARNING]
> Without this change, all nodes may respond to locally allocated addresses, producing the same effect as duplicate IP addresses on the subnet.

## BGP Port Requirements

If you plan to use BGP routing (the default installation includes the k8gobgp sidecar):

- **Port 179 (TCP)** must be available on every node where lbnodeagent runs.
- Firewalls must allow TCP 179 between cluster nodes and the upstream BGP router.
- The upstream router must be configured for BGP peering with the cluster nodes.

If you do not need BGP, install PureLB with the `-nobgp` manifest variant or set `gobgp.enabled=false` in Helm.

## Netbox Requirements

If you plan to use [Netbox IPAM integration]({{< relref "/docs/configuration/netbox" >}}):

- Network access from the allocator pod to the Netbox API.
- A Netbox API token with `ipam.view_ipaddress` and `ipam.change_ipaddress` permissions.
- A Netbox tenant configured for PureLB to allocate from.
