---
title: "Troubleshooting"
description: "Diagnose common PureLB issues by symptom."
weight: 40
---

## Service Stuck in Pending

The Service has `type: LoadBalancer` but no external IP.

**Quick diagnosis:**
```sh
kubectl purelb inspect <namespace>/<service-name>
```

**Common causes:**

1. **No ServiceGroup exists.** Check `kubectl get servicegroups -n purelb-system`. PureLB needs at least one ServiceGroup.

2. **Pool exhausted.** Check `kubectl purelb pools`. If a pool shows 0 free addresses, allocate a larger range in the ServiceGroup.

3. **Allocator not running.** Check `kubectl get pods -n purelb-system`. The allocator pod must be Running.

4. **Wrong service-group annotation.** The `purelb.io/service-group` annotation must match an existing ServiceGroup name.

5. **Requested IP outside pool.** If using `purelb.io/addresses`, verify the address is within the ServiceGroup's pool range.

## Address Not Reachable (Local Pool)

The Service has an external IP but clients cannot reach it.

**Check which node is announcing:**
```sh
kubectl describe svc <name> | grep announcing
```

**Verify the address is on the node's interface:**
```sh
# SSH to the announcing node, or use the plugin:
kubectl purelb ip addr show <interface>
```

**Check GARP:**
```sh
# If GARP is enabled, verify packets are being sent:
kubectl purelb inspect <namespace>/<service>
```

**Common causes:**

1. **Switch ARP cache stale.** After failover, the switch may still send traffic to the old node. GARP should update this, but some switches are slow. Check `purelb_lbnodeagent_garp_sent_total` and `purelb_lbnodeagent_garp_errors_total` metrics.

2. **ARP settings not configured.** Without `arp_ignore=1` and `arp_announce=2`, other nodes may respond to ARP for the VIP. See [Prerequisites]({{< relref "/docs/installation/prerequisites#arp-behavior" >}}).

3. **No pods running.** Local pools use `externalTrafficPolicy: Cluster` so all nodes forward traffic. If traffic still doesn't reach pods, check that kube-proxy is healthy.

## Address Not Reachable (Remote Pool)

The Service has an external IP on kube-lb0 but clients cannot reach it.

**Check the address is on kube-lb0:**
```sh
kubectl purelb ip addr show kube-lb0
```

**Check BGP sessions:**
```sh
kubectl purelb bgp sessions --check
```

**Check route pipeline:**
```sh
kubectl purelb bgp dataplane --check
```

**Common causes:**

1. **BGP session not established.** Verify port 179 is open, ASN values are correct, and the upstream router is configured for peering.

2. **netlinkImport not configured.** Without `netlinkImport.enabled: true` and `interfaceList: ["kube-lb0"]` in the BGPConfiguration, k8gobgp won't advertise any routes.

3. **Upstream router not accepting routes.** Some routers reject `/32` routes by default. Check the router's BGP RIB.

4. **ECMP not enabled.** The upstream router must have ECMP enabled to use multiple next-hops.

## Election Issues

**Check election health:**
```sh
kubectl purelb election --check
```

**Check Leases directly:**
```sh
kubectl get leases -n purelb-system -l app=purelb
```

**Common causes:**

1. **Node not participating.** If a node's Lease is missing or expired, check the lbnodeagent pod logs on that node.

2. **Subnet not covered.** The address's subnet must be present in at least one node's Lease annotations. Add the interface to the LBNodeAgent's `interfaces` field if needed.

3. **Frequent winner changes.** Check `purelb_election_winner_changes_total`. Frequent changes may indicate Lease renewal failures (check `purelb_election_lease_renewal_failures_total`).

## BGP Sessions Not Establishing

```sh
kubectl purelb bgp sessions --check
```

**Common causes:**

1. **Port 179 blocked.** Ensure firewall allows TCP 179 between nodes and the upstream router.

2. **ASN mismatch.** The `peerAsn` in the BGPConfiguration must match the upstream router's configured ASN.

3. **Router ID conflict.** If multiple nodes auto-detect the same router ID, set it explicitly or use `${NODE_IP}`.

4. **No BGPConfiguration CR.** k8gobgp needs a `BGPConfiguration` CR named `default` in `purelb-system`.

## Configuration Validation

Run a comprehensive configuration check:

```sh
kubectl purelb validate --strict
```

This checks for:
- Overlapping address pools across ServiceGroups
- Subnets with no nodes (unreachable pools)
- Missing BGP configuration for remote pools
- LBNodeAgent configuration consistency

## Linux Networking Tools

PureLB uses standard Linux networking. You can observe its work with standard tools:

```sh
# Show addresses on all interfaces
ip addr show

# Show addresses on the dummy interface
ip addr show dev kube-lb0

# Show the routing table
ip route show

# Show the neighbor (ARP/NDP) table
ip neigh show
```

To run these inside a lbnodeagent pod:

```sh
kubectl purelb ip addr show
kubectl purelb ip route show
```

## Logging

PureLB uses two log levels:

- **Info** -- Normal operational messages (address allocation, election changes, BGP state transitions).
- **Debug** -- Code-level troubleshooting detail (netlink calls, election hash values, GARP packet traces).

To check logs:

```sh
kubectl logs -n purelb-system deployment/allocator
kubectl logs -n purelb-system daemonset/lbnodeagent -c lbnodeagent
kubectl logs -n purelb-system daemonset/lbnodeagent -c k8gobgp
```
