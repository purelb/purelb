---
title: "Metrics Reference"
description: "Complete reference for all PureLB Prometheus metrics."
weight: 30
---

All metrics use the `purelb` namespace. Metrics are exposed on port 7472 at `/metrics` on both the Allocator and LBNodeAgent pods.

## Labeling Conventions

- `kind`: Object type (allocator-only metrics) — values: `service` (Service updates), `cache_sync` (informer sync), `pool_status` (pool status republish), `config` (ServiceGroup/LBNodeAgent config)
- `pool`: AddressPool name
- `key`: Election key (typically IP address string for allocation keys)
- `socket`: Sidecar IPC socket path
- `method`: RPC method name (Allocate, Release, Stats)
- `code`: RPC outcome code

## K8s Client Metrics (shared by Allocator and LBNodeAgent)

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_k8s_client_updates_total` | Counter | `kind` | Total K8s API updates processed. `kind` discriminates the trigger: Service/LBNodeAgent events (`service`), informer sync events (`cache_sync`), ServiceGroup status republishes (`pool_status`), or config delivery (`config`)
`purelb_k8s_client_update_errors_total` | Counter | `kind` | Failed K8s API update attempts. Only `config` errors are retried with backoff; others are logged
`purelb_k8s_client_config_loaded_bool` | Gauge | | 1 if PureLB configuration was successfully loaded at least once, 0 otherwise (stale during config errors)

## Allocator Metrics

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_address_pool_size` | Gauge | `pool` | Total number of addresses in the pool
`purelb_address_pool_addresses_in_use` | Gauge | `pool` | Number of addresses currently allocated
`purelb_address_pool_allocation_rejected_total` | Counter | `pool`, `reason` | Allocation requests rejected. `reason` is `exhausted`, `port_conflict`, `sharing_key_conflict`, `multipool_refused` (multi-pool asked of a pool that cannot serve it) or `multipool_pool_mismatch` (`purelb.io/allocated-from` disagreed with the addresses held; `pool` is `<unknown>`)
`purelb_servicegroups_out_of_scope` | Gauge | `namespace`, `name`, `type` | ServiceGroups ignored because they are not in the PureLB install namespace. `type` is `local`, `remote`, or `external` — a `remote` group being ignored withdraws its addresses from every node, a `local` one only makes its pool unallocatable
`purelb_address_pool_multipool_allocations_total` | Counter | `pool` | Multi-pool allocations performed
`purelb_address_pool_multipool_partial_total` | Counter | `pool` | Multi-pool allocations where some ranges were exhausted or had no active nodes
`purelb_address_pool_balance_pools_allocations_total` | Counter | `pool` | Balanced allocation (balancePools) allocations performed
`purelb_allocator_sg_status_writes_total` | Counter | `outcome` | ServiceGroup status writes. `outcome` is `success` or `error`
`purelb_allocator_sidecar_rpc_total` | Counter | `socket`, `method`, `code` | External IPAM sidecar RPC calls
`purelb_allocator_sidecar_rpc_duration_seconds` | Histogram | `socket`, `method` | External IPAM sidecar RPC latency

## Election Metrics (LBNodeAgent only)

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_election_lease_healthy` | Gauge | | 1 if this node's Lease is healthy and being renewed, 0 otherwise
`purelb_election_lease_renewals_total` | Counter | | Total successful Lease renewals
`purelb_election_lease_renewal_failures_total` | Counter | | Total failed Lease renewal attempts
`purelb_election_winner_changes_total` | Counter | `key` | **Breaking change (v0.17.0):** label renamed from `service` to `key`. Counts actual announcer changes per VIP (real handovers, not spurious state fluctuations). Enables diagnosis of election flapping — compare against stable baseline for your deployment
`purelb_election_member_count` | Gauge | | Current number of healthy nodes in the election
`purelb_election_subnet_count` | Gauge | | Number of unique subnets tracked across all members
`purelb_election_local_subnet_count` | Gauge | | Number of subnets on this node
`purelb_election_affinity_fallback_total` | Counter | `key` | **Breaking change (v0.17.0):** label renamed from `service` to `key`. Times a node-affinity-opted-in service had no preferred candidate eligible and fell back to standard hash election

## LBNodeAgent Local Pool Metrics

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_lbnodeagent_garp_sent_total` | Counter | | Total GARP packets sent (IPv4)
`purelb_lbnodeagent_garp_errors_total` | Counter | | Total GARP send failures
`purelb_lbnodeagent_address_renewals_total` | Counter | | Total address lifetime renewals (for non-permanent addresses)
`purelb_lbnodeagent_address_renewal_errors_total` | Counter | | Total address renewal failures
`purelb_lbnodeagent_address_additions_total` | Counter | | Total addresses added to interfaces
`purelb_lbnodeagent_address_withdrawals_total` | Counter | | Total addresses withdrawn from interfaces
`purelb_lbnodeagent_na_sent_total` | Counter | | Total Neighbor Advertisement packets sent (IPv6)
`purelb_lbnodeagent_na_errors_total` | Counter | | Total Neighbor Advertisement send failures
`purelb_lbnodeagent_announce_slot_steal_total` | Counter | `from`, `to` | Announcements stolen during preference-driven election flips (tracked for observability, not a warning condition)
`purelb_lbnodeagent_announced` | Gauge | `service`, `node`, `ip` | Currently announced addresses (1 = announced, 0 = withdrawn). Faceted per VIP, winning node, and IP family
`purelb_lbnodeagent_selector_state` | Gauge | `state` | Current workload selector state (`healthy`, `degraded`, `unhealthy`). Supplements the config_loaded_bool gauge — config_loaded stays 1 during an invalid-config outage, selector_state goes unhealthy

## LBNodeAgent Election Results

Metric | Type | Labels | Description
-------|------|--------|------------
`purelb_lbnodeagent_election_wins_total` | Counter | | Total election wins on this node (this node won at least one address)
`purelb_lbnodeagent_election_losses_total` | Counter | | Total election losses on this node (this node lost an address it previously announced)
