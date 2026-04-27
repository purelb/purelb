---
title: "Election System"
description: "How PureLB uses Kubernetes Leases to elect which node announces each local address."
weight: 30
---

When PureLB allocates a [local address]({{< relref "/docs/overview/address-types#local-addresses" >}}), only one node should announce it -- otherwise, multiple nodes would respond to ARP requests for the same IP, causing packet loss. PureLB's election system determines the winner.

Remote addresses skip the election entirely: all nodes announce them, and the upstream router handles distribution via ECMP.

## How It Works

Each LBNodeAgent creates a Kubernetes Lease object in the `purelb-system` namespace. The Lease contains:

- **Holder identity:** The node name.
- **Subnets annotation** (`purelb.io/subnets`): The list of subnets reachable from this node's interfaces.
- **Renewal timestamp:** Updated periodically to prove the node is alive.

{{< mermaid >}}
sequenceDiagram
    participant N as LBNodeAgent
    participant K as Kubernetes API
    participant L as Lease (purelb-node-X)

    N->>K: Create Lease with subnets
    loop Every retryPeriod
        N->>K: Renew Lease timestamp
        K->>L: Update renewTime
    end
    Note over N,L: Other LBNodeAgents watch all Leases<br/>to build a list of healthy nodes + subnets
{{< /mermaid >}}

## Winner Selection

When a LoadBalancer address needs to be announced, each LBNodeAgent independently determines the winner using the same deterministic algorithm:

1. **Collect healthy nodes:** Find all nodes with valid (non-expired) Leases.
2. **Filter by subnet:** Keep only nodes whose Lease subnets contain the address.
3. **Deterministic hash:** Compute SHA256 of each node name combined with the service key. Sort by hash value.
4. **First in list wins:** The node with the lowest hash announces the address.

Because every LBNodeAgent uses the same algorithm and the same Lease data, they all reach the same conclusion without coordination.

## Failover

When a node becomes unhealthy:

1. Its Lease expires (not renewed within `leaseDuration`).
2. Other nodes detect the expiry via their Lease informer.
3. The remaining nodes re-evaluate the election -- a new winner is chosen.
4. The new winner adds the address to its interface and sends Gratuitous ARP to update network switches.

## Graceful Shutdown

When a node shuts down cleanly:

1. The LBNodeAgent marks itself as unhealthy (its `Winner()` function returns empty for all addresses).
2. A `ForceSync` withdraws all addresses from the node's interfaces.
3. The Lease is deleted, so other nodes see the change immediately.
4. Remaining nodes elect new winners.

This is faster than waiting for Lease expiry and avoids connectivity gaps during planned maintenance.

## Configuration

Election timing is controlled via environment variables (set through Helm's `leaseConfig`):

Variable | Default | Description
---------|---------|------------
`PURELB_LEASE_DURATION` | `10s` | How long a Lease is valid without renewal
`PURELB_RENEW_DEADLINE` | `7s` | How long to retry renewals before giving up
`PURELB_RETRY_PERIOD` | `2s` | Interval between renewal attempts

> [!NOTE]
> The code default for `PURELB_LEASE_DURATION` is `5s`, but the Helm chart overrides it to `10s`. If installing via manifest, the deployed value is `10s`.

## Subnet-Aware Election

The election is subnet-aware: only nodes that have the address's subnet in their Lease annotations are candidates. This means:

- On a multi-subnet cluster, an address from subnet A will only be announced by a node on subnet A.
- The `interfaces` field in the [LBNodeAgent configuration]({{< relref "/docs/configuration/lbnodeagent" >}}) can add additional interfaces for subnet detection.

## Monitoring

Election health is exposed via [Prometheus metrics]({{< relref "/docs/reference/metrics#election-metrics" >}}). Key metrics: `purelb_election_lease_healthy`, `purelb_election_member_count`, and `purelb_election_winner_changes_total`.
