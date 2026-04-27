---
title: "Metrics Reference"
description: "Complete reference for all PureLB Prometheus metrics."
weight: 30
---

All metrics use the `purelb` namespace. Metrics are exposed on port 7472 at `/metrics` on both the Allocator and LBNodeAgent pods.

## Allocator Metrics

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_address_pool_size` | Gauge | `pool` | Total number of addresses in the pool
`purelb_address_pool_addresses_in_use` | Gauge | `pool` | Number of addresses currently allocated
`purelb_address_pool_allocation_rejected_total` | Counter | `pool`, `reason` | Allocation requests rejected (exhaustion, sharing constraints, etc.)
`purelb_address_pool_multipool_allocations_total` | Counter | `pool` | Multi-pool allocations performed
`purelb_address_pool_multipool_partial_total` | Counter | `pool` | Multi-pool allocations where some ranges were exhausted or had no active nodes
`purelb_address_pool_balance_pools_allocations_total` | Counter | `pool` | Balanced allocation (balancePools) allocations performed

## Election Metrics

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_election_lease_healthy` | Gauge | | 1 if this node's Lease is healthy and being renewed, 0 otherwise
`purelb_election_lease_renewals_total` | Counter | | Total successful Lease renewals
`purelb_election_lease_renewal_failures_total` | Counter | | Total failed Lease renewal attempts
`purelb_election_winner_changes_total` | Counter | `service` | Number of winner changes (indicates failover or rebalancing)
`purelb_election_member_count` | Gauge | | Current number of healthy nodes in the election
`purelb_election_subnet_count` | Gauge | | Number of unique subnets tracked across all members
`purelb_election_local_subnet_count` | Gauge | | Number of subnets on this node

## LBNodeAgent Metrics

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_lbnodeagent_garp_sent_total` | Counter | | Total GARP packets sent
`purelb_lbnodeagent_garp_errors_total` | Counter | | Total GARP send failures
`purelb_lbnodeagent_address_renewals_total` | Counter | | Total address lifetime renewals (for non-permanent addresses)
`purelb_lbnodeagent_address_renewal_errors_total` | Counter | | Total address renewal failures
`purelb_lbnodeagent_address_additions_total` | Counter | | Total addresses added to interfaces
`purelb_lbnodeagent_address_withdrawals_total` | Counter | | Total addresses withdrawn from interfaces
`purelb_lbnodeagent_election_wins_total` | Counter | | Total election wins on this node
`purelb_lbnodeagent_election_losses_total` | Counter | | Total election losses on this node
