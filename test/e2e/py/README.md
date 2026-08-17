# PureLB end-to-end tests

176 pytest tests that run against a **real cluster with PureLB already
installed**. They create and delete Services, ServiceGroups and
LBNodeAgents, taint nodes, and kill lbnodeagent pods to force failover,
then verify the result by reading the node's interfaces over SSH, the
Prometheus counters, the pod logs and — where a router is available —
the frames on the wire.

This is destructive. `--context` is required and has no default, so it
can never act on whatever kubectl happens to point at.

---

## 1. What you need

The cluster's required shape — node count, subnets, IPv6, the second NIC,
the address bands that must be free — is in
[../README.md](../README.md#the-cluster-the-suite-needs), with a diagram.
What follows is what the suite needs from *you* to run against it.

**Required for essentially every test:**

| | |
|---|---|
| A cluster with PureLB v0.17.0+ installed | namespace `purelb-system` unless you pass `--purelb-namespace` |
| A kubectl context for it | passed as `--context <name>`; no default |
| An nginx backend in namespace `test` | every Service the suite creates selects `app: nginx` there. `reset-test-cluster.sh` applies [nginx-test.yaml](../nginx-test.yaml) if it is missing |
| Passwordless SSH from this workstation to every node's InternalIP | the assertions read `ip -br addr show` on the node — "did the VIP land on the NIC" is not answerable from the API |
| Reachable `:7472` on each node | lbnodeagent is hostNetwork and is scraped directly. The allocator is scraped through the apiserver proxy, so it needs no route |

**Optional — each unlocks specific modules:**

| | Unlocks | Without it |
|---|---|---|
| `--router-host <host>` — SSH to a router running FRR, with `vtysh` and `tcpdump`, and passwordless `sudo` for the capture | `test_router_bgp.py`, `test_local_garp.py` | skips, visibly |
| `./kubectl-purelb` built by `make plugin` at the repo root | `test_plugin_validate.py` | skips, visibly |
| `PURELB_TEST_UPGRADE=1` | `test_upgrade.py` — see §6, it degrades the cluster while it runs | skips, visibly |
| An external-IPAM sidecar image — see [EXTERNAL-IPAM.md](EXTERNAL-IPAM.md) | `test_ipam_external.py` | **errors**: the allocator rollout never completes. Deselect the module |

---

## 2. Install

```bash
cd test/e2e/py
python3 -m venv .venv && .venv/bin/pip install -e .
```

Check the install without touching a cluster — the harness self-checks
that need no cluster:

```bash
.venv/bin/pytest tests/test_harness.py -q \
  -k "not connects and not pod_resolution and not metrics_scrape \
      and not logs_are_windowed and not capability_probe and not router_reachable"
```

```
................                                                         [100%]
16 passed, 7 deselected in 0.47s
```

---

## 3. Run

Reset the cluster to a known baseline first, then run:

```bash

.venv/bin/pytest --context <ctx> [--router-host <frr-host>]
```

The reset is not optional housekeeping. A leftover ServiceGroup whose
ranges overlap the ones a fixture is about to create is **rejected by the
allocator**, and a stray LoadBalancer Service holding an address from the
generated pool skews the `addresses_in_use` deltas the tests assert on.
It removes every Gateway (its controller would recreate the Service),
every LoadBalancer Service outside the install namespace, every
ServiceGroup and LBNodeAgent — restoring the shipped `default` agent —
the scratch namespaces `test-tenant` and `echo-test`, and any
`purelb-test` NoExecute taints. It then verifies no PureLB address is
still configured on any node.

It prints everything it will delete and prompts before deleting any of
it, which is worth reading rather than reflexively passing `--yes`:
"every ServiceGroup" and "every LoadBalancer Service outside the install
namespace" includes fixtures you made by hand and want to keep. Back
those up first.

Useful flags:

| Flag | |
|---|---|
| `--context` | kubectl context. **Required.** |
| `--purelb-namespace` | install namespace (default `purelb-system`) |
| `--router-host` | upstream router; enables the BGP and on-the-wire GARP/NA modules |
| `--require a,b` | a missing capability **fails** instead of skipping — for release runs |
| `--show-tests` | every test with its result and **what it checked**, instead of progress dots |
| `--report PATH` | write a plain-text report of the whole run to `PATH` |
| `-x` | stop at the first failure |
| `--durations=20` | the 20 slowest tests, which is how to find out what a full run costs on your cluster |

Every run ends with a `SKIPPED — this run did NOT test the following`
block. Read it. Green with half the suite skipped is not the same as
green.

### Seeing what actually ran

By default pytest prints one dot per test, which tells you nothing about
what was verified. `--show-tests` replaces the dots with a line per test
**as it finishes** — the verdict and what the test actually checked,
taken from the test's own docstring. Several tests here take minutes
apiece (restarting a DaemonSet, waiting out a lease, capturing on the
wire), so watching it run matters:

```
$ .venv/bin/pytest --context <ctx> --show-tests
tests/test_local_policy.py::test_etp_local_is_overridden_for_a_local_pool PASS  ExternalTrafficPolicy: Local is forced to Cluster on local pools. [ 12%]
tests/test_local_policy.py::test_a_pool_matching_no_node_allocates_but_announces_nothing PASS  An address nobody can announce is still allocated. [ 25%]
```

The run then ends with a recap grouped by module, so a failure part way
through a 20-minute run does not have to be found by scrolling:

```
=================================== RESULTS ====================================
test_local_policy.py
  PASS     6.4s  ExternalTrafficPolicy: Local is forced to Cluster on local pools.
  FAIL    12.0s  The allow-local annotation is honoured.
                 AssertionError: ETP is 'Cluster'; purelb.io/allow-local must preserve Local
  SKIP     0.0s  BGP routes are learned by the upstream router.
```

`--report PATH` writes the same to a file, with the **full** failure
output rather than the one-line summary, plus the context, namespace,
router and the capabilities the probe detected — which is what makes a
skip list actionable. That is the artifact to attach to a ticket or keep
beside a release:

```
.venv/bin/pytest --context <ctx> --router-host <frr> \
    --report /tmp/purelb-e2e-$(date +%F).txt
```

Both work with any other flags, and neither changes what runs.

---

## 4. Every test, by module

176 tests in 18 modules. A row marked `[cap]` needs that capability and
skips without it (§6); `×N` means the test is parametrized and counts N
times. Names are node IDs — pass any of them to `pytest` directly.

### Local mode — the default install

The VIP is configured on the node's **physical** interface and announced
with ARP/NDP. One node wins the election and announces.

#### [test_local_allocation.py](tests/test_local_allocation.py) — 10 tests

The core path. The three families were three near-identical bash
functions that had drifted; parametrizing makes their parity structural.

| Test | Asserts |
|---|---|
| `test_allocates_announces_and_serves` ×3 — `v4`, `v6`, `dual` | One address per family, on a real interface, serving traffic — and not on `kube-lb0` |
| `test_release_withdraws_the_address` | Deleting the Service frees the address and takes it off the node |
| `test_specific_ip_request_is_honoured` | `spec.loadBalancerIP` gets exactly that address, and it announces |
| `test_foreign_loadbalancer_class_is_ignored` | A Service for another controller's class is left alone |
| `test_explicit_purelb_loadbalancer_class_allocates` | The other half: naming PureLB's own class explicitly must work |
| `test_shared_ip_puts_two_services_on_one_address` | Two Services with the same sharing key share one VIP |
| `test_multiple_services_get_distinct_addresses` | No two Services may be handed the same address |
| `test_no_duplicate_vips_across_nodes` | Exactly one node announces each address |

#### [test_local_address_lifetime.py](tests/test_local_address_lifetime.py) — 6 tests

How the address is configured on the interface, and why each property
matters.

| Test | Asserts |
|---|---|
| `test_local_vip_has_a_finite_lifetime` | A permanent VIP would survive the death of the node announcing it |
| `test_local_vip_has_noprefixroute` | Without it the kernel routes the whole subnet via the VIP |
| `test_ipv6_vip_does_dad_unless_told_otherwise` `[ipv6]` | DAD is on by default — it is what stops two nodes announcing one address after a split brain |
| `test_skip_ipv6_dad_sets_nodad_when_enabled` `[ipv6]` | The opt-in works, and is visible both on the wire and in the API |
| `test_address_lifetime_is_renewed_rather_than_expiring` | The VIP survives its lifetime, and the agent's timer is what keeps it alive |
| `test_cni_does_not_adopt_a_vip_as_the_node_address` | A CNI must not pick a VIP as the node's own address |

#### [test_local_failover.py](tests/test_local_failover.py) — 3 tests

Applies and removes `NoExecute` taints; the fixture guarantees removal
whatever the test does.

| Test | Asserts |
|---|---|
| `test_vip_fails_over_when_its_announcer_is_evicted` `[multi-node]` | Evict the announcing agent; the VIP moves and keeps serving |
| `test_agents_recover_after_the_taint_is_removed` `[multi-node]` | A tainted-out agent comes back and the DaemonSet is whole again |
| `test_failover_stays_within_the_addresss_subnet` `[multi-subnet]` | A VIP may only move to a node on its own subnet |

#### [test_local_multipool.py](tests/test_local_multipool.py) — 9 tests

Subnet placement, and multi-pool — which only makes sense given it.

| Test | Asserts |
|---|---|
| `test_default_group_places_each_vip_on_its_own_subnet` | The default group spans both subnets; placement must still match |
| `test_ipv6_vips_are_placed_on_their_own_subnet` `[ipv6]` | The same rule for IPv6, which NDP makes just as subnet-local |
| `test_leases_carry_the_subnet_of_their_node` | Election is subnet-aware, and the leases are how it knows |
| `test_servicegroup_multipool_gives_one_address_per_subnet` ×3 — `v4`, `v6`, `dual` | `multiPool: true` → one address per subnet range, per family |
| `test_multipool_annotation_enables_it_on_a_single_pool_group` | The annotation turns multi-pool on for a group that does not set it |
| `test_multipool_annotation_overrides_the_group_and_turns_it_off` | …and off again, the direction that could silently fail |
| `test_vip_has_no_home_when_its_subnet_runs_out_of_nodes` `[multi-node]` | Exhaust a subnet's nodes and the VIP must go **nowhere**, not elsewhere |

#### [test_local_balanced.py](tests/test_local_balanced.py) — 7 tests

`balancePools`, and its mutual exclusion with `multiPool`.

| Test | Asserts |
|---|---|
| `test_allocations_are_spread_evenly_across_ranges` ×3 — `v4`, `v6`, `dual` | Each range gets its fair share, per family independently |
| `test_balanced_addresses_are_announced_and_serve` | A balanced address is a normal address: announced, and it works |
| `test_balance_pools_and_multi_pool_together_are_refused` | A group asking for both is asking for two contradictory things |
| `test_multi_pool_annotation_on_a_balanced_group_is_refused` | The same conflict via the annotation route |
| `test_naming_a_servicegroup_that_does_not_exist_is_refused` | An unknown group is a distinct failure from an unusable one |

#### [test_local_policy.py](tests/test_local_policy.py) — 8 tests

Policy, pool edge cases and end-to-end traffic. Two assert that
something does **not** happen, which is where the value is.

| Test | Asserts |
|---|---|
| `test_pool_with_no_matching_subnet_allocates_but_announces_nowhere` | Subnet-aware election with no candidate must not fall back to "some node" |
| `test_idle_servicegroup_publishes_status` | A pool reports its capacity before anything is allocated from it |
| `test_etp_local_is_overridden_for_a_local_pool` | `ExternalTrafficPolicy: Local` is forced to `Cluster` on local pools |
| `test_allow_local_annotation_preserves_etp_local` | The opt-out is honoured, so the override is a policy and not a bug |
| `test_re_evaluate_annotation_is_consumed` | The one-shot trigger is cleared, and the address survives it |
| `test_graceful_shutdown_releases_the_lease` `[multi-node]` | An agent stopped cleanly gives its lease up rather than expiring |
| `test_traffic_reaches_pods_on_nodes_other_than_the_announcer` `[multi-node]` | The VIP load-balances beyond the node holding it |
| `test_ipv6_vip_serves_traffic` `[ipv6]` | The IPv6 data path, end to end |

#### [test_local_multi_interface.py](tests/test_local_multi_interface.py) — 10 tests

Dual-homed nodes and interface selection. The bash equivalent needed six
hand-set environment variables and so never ran at all.

| Test | Asserts |
|---|---|
| `test_second_nic_is_not_advertised_by_default` | The baseline every other test here depends on: only the default-route NIC is advertised |
| `test_scoped_agent_adds_the_second_nics_subnets` | A `nodeSelector`-scoped LBNodeAgent widens exactly one node's lease |
| `test_vip_on_the_second_subnet_announces_on_that_nic` | A dual-stack VIP from the second subnet lands on the second NIC |
| `test_config_shrink_withdraws_and_kills_the_renewal_timer` | Removing the scoped CR withdraws the address, timer and all |
| `test_a_deselected_node_advertises_nothing` `[multi-node]` | `selector_state=deselected` — a node no CR selects must announce nothing, or it wins elections it cannot serve |
| `test_an_agent_with_no_local_spec_leaves_the_node_on_default_detection` `[multi-node]` | `selector_state=default` — agents matched, none configured anything |
| `test_an_uncompilable_local_interface_silences_only_that_node` `[multi-node]` | `selector_state=invalid` — the announcer refused the config, and only that node goes quiet |
| `test_affinity_placed_address_announces_itself` | A node that announces an address must gratuitously announce it |
| `test_affinity_follows_the_endpoints_for_every_family` `[multi-node]` | The address lands where the endpoints are — IPv4 **and** IPv6 |
| `test_affinity_falls_back_and_counts_it_when_no_preferred_node_can_announce` `[multi-node, multi-subnet]` | Endpoints somewhere the address cannot be announced from: fall back, and count it |

#### [test_local_remaining.py](tests/test_local_remaining.py) — 7 tests

Remote pools from the local suite's angle, election losers, and traffic
from inside the cluster.

| Test | Asserts |
|---|---|
| `test_remote_address_lands_on_the_dummy_interface_on_every_node` | Remote is the opposite of local: everywhere, on `kube-lb0` |
| `test_idle_remote_pool_publishes_status_for_both_families` | Capacity is reported before anything allocates, per family |
| `test_losers_report_losing_and_nothing_panics` | The nodes that did **not** win must say so, and stay healthy |
| `test_one_vip_spreads_across_backend_pods` | A single VIP reaches several backends, not just the nearest |
| `test_a_pod_can_reach_a_vip` | Reachable from inside the cluster network, not only from a node |
| `test_a_service_picks_up_a_range_added_later` `[multi-subnet]` | `multiPool` is not evaluated only at creation time |
| `test_a_node_can_reach_a_vip_on_the_other_subnet` `[multi-subnet]` | Traffic crosses the subnet boundary and back |

#### [test_local_garp.py](tests/test_local_garp.py) — 4 tests

The only module that asks the **network** rather than the cluster:
frames captured with `tcpdump` on the router. A gratuitous ARP that is
counted but never leaves the node is, from inside the node,
indistinguishable from one that worked.

| Test | Asserts |
|---|---|
| `test_announcing_an_ipv4_address_puts_gratuitous_arp_on_the_wire` `[router]` | The frames reach the router, in both forms (Request and Reply), from the right node's MAC |
| `test_announcing_an_ipv6_address_puts_an_unsolicited_na_on_the_wire` `[router, ipv6]` | The IPv6 counterpart, and the only on-wire proof it exists |
| `test_a_moved_address_is_re_announced_by_the_new_winner` `[router, multi-node]` | Failover is the case gratuitous announcement exists for |
| `test_disabling_garp_stops_the_announcements` `[router]` | The control that makes the three above mean anything |

### Remote mode and BGP

The address goes on the **kube-lb0 dummy on every node** and is reached
by routing, not ARP. No election winner, and the pool need not be inside
any node subnet.

#### [test_remote.py](tests/test_remote.py) — 30 tests

The bulk is `ExternalTrafficPolicy: Local`, which is coherent here — every
node announces, so honouring it strands nothing — and each endpoint
transition is a chance to strand an address. The rest is rejection.

| Test | Asserts |
|---|---|
| `test_every_node_announces_on_the_dummy_interface` | No election, no winner: all nodes, on `kube-lb0` |
| `test_dual_stack_remote_allocation` `[ipv6]` | One address per family, both on `kube-lb0` everywhere |
| `test_remote_address_flags_on_the_dummy_interface` | A remote VIP is **permanent**, unlike a local one |
| `test_local_and_remote_pools_do_not_contaminate_each_other` | Two Services, two modes, each address only where it belongs |
| `test_etp_local_restricts_announcement_to_endpoint_nodes` | With ETP Local, only nodes holding an endpoint announce |
| `test_etp_local_withdraws_when_the_last_endpoint_goes` | Zero endpoints means nowhere — and back when one returns |
| `test_etp_local_follows_the_endpoint_to_another_node` `[multi-node]` | Move the endpoint; the announcement moves with it |
| `test_switching_a_service_to_etp_local_narrows_the_announcement` | `Cluster` → `Local` on a live Service, without losing the address |
| `test_requesting_an_address_outside_the_pool_is_refused` | A typo in `purelb.io/addresses` must not silently allocate something else |
| `test_requesting_an_address_already_in_use_is_refused` | Two Services on one address, with no sharing key, is a collision |
| `test_pool_exhaustion_is_reported` | One more Service than the pool has addresses |
| `test_sharing_key_added_to_a_live_service` | Sharing is decided at allocation time, not retroactively |
| `test_single_stack_service_upgraded_to_dual_stack` `[ipv6]` | Adding IPv6 to a live Service allocates the second family |
| `test_re_evaluate_on_a_remote_service` | The trigger is consumed and the address survives |
| `test_deleting_a_remote_service_withdraws_it_from_every_node` | Withdrawal has to reach every node, not just one |
| `test_a_remote_address_survives_losing_one_node` `[multi-node]` | Every node announces, so losing one changes nothing for the rest |
| `test_a_restarted_agent_re_announces` | An agent that restarts must put the address back |
| `test_remote_multi_pool_allocates_from_every_range` `[multi-subnet]` | `multiPool` works for remote groups too, and is not subnet-gated |
| `test_aggregation_sets_the_configured_prefix_on_every_node` ×4 — `v4-host-route`, `v4-default-subnet`, `v6-host-route`, `v6-default-subnet` | The mask is what was asked for, identically on every node |
| `test_ipv6_only_remote_allocation` `[ipv6]` | IPv6 on its own, not only as half of a dual-stack Service |
| `test_remote_addresses_can_be_shared` | Two Services, one remote address, different ports |
| `test_a_specific_remote_address_can_be_requested` | `purelb.io/addresses` gets exactly that address, and it announces |
| `test_an_agent_restart_during_etp_local_restores_the_narrow_set` | Restarting an agent must not widen an ETP Local announcement |
| `test_remote_service_picks_up_a_range_added_later` | Incremental multi-pool for remote groups |
| `test_a_malformed_aggregation_is_rejected_at_admission` | The API server refuses an aggregation the announcer cannot parse |
| `test_a_remote_vip_serves_traffic_from_a_pod` | Reachability for a remote VIP, tested from a pod rather than a node |
| `test_a_remote_ipv6_vip_serves_traffic_from_a_pod` `[ipv6]` | The same for IPv6, which fails independently |

#### [test_router_bgp.py](tests/test_router_bgp.py) — 17 tests

What the **router** learned. Routes are read from FRR's RIB as JSON, and
next-hops count only those FRR reports as active or fib — a route can
list next-hops it has not installed, and those forward nothing. The
peering check is first, so a peering problem reports itself instead of
surfacing as sixteen mysterious route failures. Every test needs
`[router]`.

| Test | Asserts |
|---|---|
| `test_the_router_is_peered_with_every_node` | Established sessions with all of them, before anything else runs |
| `test_the_router_learns_a_host_route_via_every_node` ×2 — `IPv4`, `IPv6` | A VIP becomes a /32 (or /128) in the RIB, ECMP over all nodes |
| `test_the_advertised_prefix_length_matches_the_aggregation` ×2 — `host-route`, `subnet-aggregate` | Aggregation decides what the router learns, which is the point of it |
| `test_deleting_the_service_withdraws_the_route` | The route goes when the Service does |
| `test_losing_a_node_drops_only_its_next_hop` `[multi-node]` | One node down means one next-hop fewer, not a withdrawn route |
| `test_etp_local_narrows_the_next_hops_to_endpoint_nodes` | ETP Local is visible in the RIB, not only on the interfaces |
| `test_two_services_sharing_an_address_produce_one_route` | A shared address is one route, and it survives losing one holder |
| `test_the_vip_is_reachable_from_outside_the_cluster` ×2 — `IPv4`, `IPv6` | Traffic from off-cluster follows the advertised route to a pod — the only end-to-end proof in the suite |
| `test_etp_local_next_hops_track_the_endpoint_count` | Scale the backend and the router's next-hops follow |
| `test_aggregation_advertises_one_prefix_and_not_the_other` ×2 — `host-route-only`, `aggregate-only` | The prefix that was asked for, and **not** the one that was not |
| `test_an_aggregate_route_survives_losing_one_of_its_services` | With `default` aggregation, two Services share one /24 route |
| `test_a_withdrawn_vip_stops_serving_from_outside` | Withdrawal has to actually stop traffic, not just tidy the RIB |
| `test_next_hops_are_restored_when_a_node_comes_back` `[multi-node]` | Recovery, not just failure |

### Features that cut across both modes

#### [test_namespace_scoping.py](tests/test_namespace_scoping.py) — 11 tests

The v0.17.0 headline feature. Creates and removes the `test-tenant`
namespace. This is an allocation control, **not** RBAC: PureLB ships no
admission webhook, so a refused Service is created quite happily and
simply never receives an address — which is the shape these tests assert.

| Test | Asserts |
|---|---|
| `test_bound_namespaces_is_published_in_status` | `status.boundNamespaces` is how an operator sees what was parsed |
| `test_unannotated_service_in_the_namespace_uses_its_group` | The whole point: no annotation, and it still lands in the tenant pool |
| `test_a_service_elsewhere_still_gets_the_default_group` | Binding one namespace must not change any other |
| `test_without_enforcement_an_annotation_still_reaches_in` | `namespaces` alone is a **default**, not a boundary |
| `test_dual_stack_from_a_namespace_scoped_group` `[ipv6]` | One address per family, both from the tenant's own pools |
| `test_enforcement_refuses_an_outsider` | Outsiders cannot reach in, even by naming the group explicitly |
| `test_enforcement_also_refuses_an_insider_reaching_out` | The other direction — the half that is easy to forget |
| `test_enforcement_on_one_group_fences_the_whole_namespace` | Enforcement belongs to the **namespace**, not to the group that set it |
| `test_namespace_default_picks_between_two_groups` | Where two groups serve a namespace, the marked one wins |
| `test_ambiguous_default_without_enforcement_falls_back_and_warns` | Two groups, neither marked: fall through to `default` and warn |
| `test_ambiguous_default_with_enforcement_is_denied` | Enforcing, the same ambiguity is a denial rather than a fallback |

### Operational

#### [test_plugin_validate.py](tests/test_plugin_validate.py) — 7 tests

`kubectl purelb validate`, driven as a subprocess and asserted on its
JSON and exit code. Mostly about misconfiguration: each test creates a
config PureLB will quietly not serve and asserts validate says so.
Skips unless `make plugin` has been run.

| Test | Asserts |
|---|---|
| `test_a_healthy_cluster_validates_clean` | Zero failures on a working config, and exit 0 |
| `test_a_servicegroup_outside_the_install_namespace_is_reported` | PureLB ignores it entirely, which is invisible without this check |
| `test_an_ambiguous_namespace_binding_is_reported` | Two groups serve a namespace and neither is its default |
| `test_overlapping_pool_ranges_are_reported` | Two groups claiming the same addresses — names which pair collide |
| `test_a_pool_no_node_can_announce_is_reported` | A local pool whose subnet matches no node |
| `test_a_node_selector_matching_nothing_is_reported` | An LBNodeAgent whose selector matches no node has no effect |
| `test_strict_mode_turns_a_warning_into_a_failure` | `--strict` is what makes this usable in CI |

#### [test_timing.py](tests/test_timing.py) — 4 tests

Order-of-magnitude tripwires, not performance targets. Each is sampled
`TIMING_SAMPLES` times (3) and judged on its **worst** sample — averaging
is how you hide a race. Ceilings are overridable per §6.

| Test | Asserts |
|---|---|
| `test_b1_service_creation_to_vip_on_the_interface` | Create → the address exists on a node, under 5s |
| `test_b3_service_deletion_to_vip_withdrawn` | Delete → the address has left every node, under 5s |
| `test_d3_service_creation_to_first_successful_request` | Create → it actually serves traffic, under 15s |
| `test_e3_election_reconvergence_after_losing_a_node` `[multi-node]` | Announcer gone → another node holds the address, under 30s |

**This module emits an artifact, and that is half its value.** Every run
appends a `timing-results-<timestamp>.csv` to [../timing/](../timing/), in the
same `test,event,value` format as the nine committed baselines going back to
2026-01-21 — per-iteration rows in milliseconds plus a summary row
(`count,min,max,avg,p95`). The ceilings catch a structural regression; the
series catches drift, which is the thing no assertion can see.

New results are gitignored, so committing one is a deliberate act: do it when
you want to move the baseline. If you refactor or re-port this module, **check
it still writes the file** — a previous migration kept every assertion passing
while silently dropping the artifact, and nothing noticed.

#### [test_stress_failover.py](tests/test_stress_failover.py) — 1 test

| Test | Asserts |
|---|---|
| `test_repeated_failover_never_strands_or_duplicates_the_address` `[multi-node]` | Fail the announcer over and over — grace 0 (SIGKILL) and 15 (clean withdrawal), under background churn — and check the invariants each time. `PURELB_STRESS_ITERATIONS` (3) |

#### [test_upgrade.py](tests/test_upgrade.py) — 2 tests

Opt-in with `PURELB_TEST_UPGRADE=1`: these remove the status subresource
from the live ServiceGroup CRD and put it back, which degrades status
reporting cluster-wide while they run.

| Test | Asserts |
|---|---|
| `test_a_crd_without_a_status_subresource_breaks_status_but_not_allocation` | Allocation and announcement keep working; only status reporting is lost — which is why nobody notices |
| `test_applying_the_new_crd_restores_status_reporting` | The remedy in the release notes actually works |

### The harness itself

#### [test_harness.py](tests/test_harness.py) — 23 tests

These test the code that decides whether PureLB works. 16 need no
cluster; 7 do.

| Test | Asserts |
|---|---|
| `test_parses_labels_and_values` | Labelled and unlabelled series parse to the right values |
| `test_absent_and_zero_are_distinguishable` | `get` returns None for absent; `counter` folds it to 0 |
| `test_counter_matches_a_label_subset` | A test must not have to name labels it does not care about |
| `test_counter_does_not_match_a_label_value_by_prefix` | `pool="default"` must not aggregate `pool="default-v6"` |
| `test_increase_assertions` | A delta of 0 fails, and `min_delta` is enforced — the `> 0` bug made unrepresentable |
| `test_increase_on_an_unknown_metric_name_is_a_typo_not_a_flat_counter` | A metric name that exists nowhere must not read as "did not move" |
| `test_nearest_names_help_find_a_renamed_metric` | A miss suggests the near-miss names, so a rename is diagnosable |
| `test_not_increased_tolerates_history` | A pre-existing nonzero error count must not fail a later run |
| `test_scrape_failure_raises_rather_than_returning_empty` | The failure that used to skip assertions silently |
| `test_wait_until_returns_value_and_times_out` | Returns the predicate's value; raises `WaitTimeout` with the description |
| `test_wait_until_surfaces_the_last_error` | A predicate that keeps raising must not be reported as a bare timeout |
| `test_announcing_splits_on_first_and_last_comma` | Interface names may legally contain commas |
| `test_announcing_drops_malformed_entries_rather_than_raising` | Leniency is the product's behaviour, so it is the harness's too |
| `test_announcing_is_keyed_by_ip` | The IP is the slot key, which is the v0.17.0 fix |
| `test_announcing_has_node_is_entry_wise` | Substring matching cannot tell `purelb2-1` from `purelb2-10` |
| `test_announcing_normalises_ipv6` | Comparisons are on parsed addresses, not on text |
| `test_connects_and_sees_nodes` | The cluster answers, and every node has an InternalIP |
| `test_pod_resolution_is_exact` | One node resolves to exactly one pod, by field selector |
| `test_allocator_metrics_scrape` | The allocator scrape works and looks like PureLB |
| `test_agent_metrics_scrape` | An lbnodeagent scrape works and its lease is healthy |
| `test_logs_are_windowed` | Reading with a window must not return the whole history |
| `test_capability_probe_agrees_with_the_cluster` `[multi-node]` | The probe's `multi-node` verdict matches the node count |
| `test_router_reachable_and_faces_the_pool_subnet` `[router]` | The router answers and has a real interface on the node subnet |

### External IPAM

#### [test_ipam_external.py](tests/test_ipam_external.py) — 17 tests

A sidecar hands out addresses over the gRPC IPAM contract. Needs a
sidecar image — see [EXTERNAL-IPAM.md](EXTERNAL-IPAM.md); without one the
allocator rollout never completes and every test here errors. These run
in file order and share module-scoped fixtures: allocation happens once,
several tests assert on it, and the release test tears it down last.

| Test | Asserts |
|---|---|
| `test_allocator_can_write_servicegroup_status` | The v0.17.0 status subresource must be writable |
| `test_sidecar_is_running_alongside_the_allocator` | The sidecar container is in the allocator pod and every container is ready |
| `test_external_servicegroup_is_accepted` | The `external` ServiceGroup exists and carries the provider |
| `test_backend_is_ready` | The nginx backend has a ready replica before anything is measured |
| `test_address_comes_from_the_sidecar_pool` | Exactly one address, and it is inside the sidecar's CIDR |
| `test_pool_type_annotation_matches_the_announce_mode` | `purelb.io/pool-type` matches `local` or `remote` as configured |
| `test_address_is_announced_on_a_node_interface` (local only) | The VIP reaches a real node interface, not `lo` |
| `test_vip_serves_the_backend` (local only) | HTTP from a node, which is what a client would do |
| `test_status_reflects_sidecar_stats` | `.status.ipam` names the provider and the allocated count comes from `Stats` |
| `test_allocate_rpc_was_made` | The `Allocate` RPC advanced with `code="OK"`, as a delta |
| `test_stats_rpc_was_made` | The `Stats` RPC advanced with `code="OK"`, as a delta |
| `test_no_sidecar_rpc_failed` | No non-OK RPC, compared as a delta so old errors cannot fail a run |
| `test_release_withdraws_the_address_and_calls_the_sidecar` | Deleting withdraws the address from every node and `Release` is called |
| `test_ipv6_only_allocation_from_the_sidecar` | One address, inside the sidecar's IPv6 CIDR |
| `test_dual_stack_allocation_from_the_sidecar` | One address per family, each from its own pool |
| `test_dual_stack_status_counts_both_families` | `status.allocatedIPv6` moves, so the sidecar's `in_use_v6` reaches the CR |
| `test_cleanup_leaves_no_residue` | Every Service this module created is gone, and so are its addresses |

## 5. Running a subset

```bash
# Everything except external IPAM — the usual run
.venv/bin/pytest --context <ctx> --ignore=tests/test_ipam_external.py

# One module
.venv/bin/pytest tests/test_local_allocation.py --context <ctx>

# One test, verbose, stop on failure
.venv/bin/pytest tests/test_local_failover.py::test_vip_fails_over_when_its_announcer_is_evicted \
    --context <ctx> -x -v

# Local mode only
.venv/bin/pytest tests/test_local_*.py --context <ctx>

# By name
.venv/bin/pytest --context <ctx> -k "dual_stack or ipv6"

# A release run: skips become failures
.venv/bin/pytest --context <ctx> --router-host <frr> \
    --require multi-subnet,ipv6,multi-node,router,dual-homed
```

`test_ipam_external.py` runs in file order and shares module-scoped
fixtures — allocation happens once, several tests assert on it, and the
release test tears it down last. Selecting a single test out of that
module still works, but selecting the *middle* of the sequence will
re-run its setup. Every other module is order-independent.

---

## 6. Capabilities, skips, and the modules that ask for something extra

The conftest probes the cluster once per session, over SSH, and gates
tests with `@pytest.mark.requires(...)`:

| Capability | True when |
|---|---|
| `multi-node` | 2+ nodes |
| `multi-subnet` | node IPs span 2+ /24s |
| `ipv6` | a node carries a non-link-local IPv6 address |
| `dual-homed` | a node has more than one `eth*` interface |
| `router` | `--router-host` was given and `tcpdump` is present on it |

A missing capability skips, and the skip is listed at the end of the run.
`--require multi-subnet,ipv6` turns those two into failures instead.

**The router modules** (`test_router_bgp.py`, `test_local_garp.py`) are
the only ones that ask the network rather than the cluster. An address
can be perfectly placed on `kube-lb0` on every node and advertise nothing
to anyone; a gratuitous ARP that is counted but never leaves the node is
indistinguishable, from inside the node, from one that worked. For IPv6
it matters most: the kernel does not notify neighbours when an address is
added, so without PureLB's unsolicited NA a moved IPv6 VIP converges only
on NUD timeouts.

```
this workstation
    |  curl to VIPs — the only end-to-end proof in the suite
    v
FRR router  <-- --router-host
    |  BGP routes, ECMP over every node
    v
cluster nodes running gobgpd
```

Two things worth knowing before you read a GARP failure. `garpConfig` is
optional with no default and `sendGARPSequence` returns immediately when
it is nil — **as shipped, PureLB sends nothing gratuitously** — so the
fixture patches the `default` LBNodeAgent and restores it. And one
`sendGARP` call puts *two* frames on the wire, an ARP Request and an ARP
Reply, so frame counts run at 2× the metric.

Minimum FRR config for the BGP module, with ECMP so a VIP gets one
next-hop per node:

```
router bgp 64514
 bgp router-id 172.30.255.10
 no bgp ebgp-requires-policy
 neighbor <node> remote-as 64515
 address-family ipv4 unicast
  neighbor <node> activate
  maximum-paths 8          ! without this there is no ECMP to assert
 exit-address-family
```

The first test in the module checks the BGP session, so a peering problem
reports itself instead of surfacing as sixteen mysterious route failures.
By hand:

```bash
ssh $ROUTER "sudo vtysh -c 'show bgp summary'"
ssh $ROUTER "sudo vtysh -c 'show ip route 10.255.0.0/24 longer-prefixes'"
```

Note the leading slash in `aggregation: /32` and `/128`. `addVirtualInt`
builds the mask with `net.ParseCIDR("0.0.0.0" + aggregation)`, so `"32"`
becomes `"0.0.0.032"` and fails; a CRD Pattern now rejects it at
admission.

**`test_upgrade.py`** removes the status subresource from the live
ServiceGroup CRD and puts it back. That is a real cluster-wide change for
the duration — every ServiceGroup loses status reporting while it runs.
The restore is in a `finally` and is verified, but a test that can leave
a cluster degraded should be asked for:

```bash
PURELB_TEST_UPGRADE=1 .venv/bin/pytest tests/test_upgrade.py --context <ctx>
```

**`test_stress_failover.py`** defaults to 3 iterations so it fits an
ordinary run. For a soak:

```bash
PURELB_STRESS_ITERATIONS=20 .venv/bin/pytest tests/test_stress_failover.py --context <ctx>
```

**`test_timing.py`** ceilings are `TIMING_MAX_B1_MS` (create → VIP on the
NIC, 5000), `TIMING_MAX_B3_MS` (delete → withdrawn, 5000),
`TIMING_MAX_D3_MS` (create → first good curl, 15000),
`TIMING_MAX_E3_MS` (re-convergence after node loss, 30000), sampled
`TIMING_SAMPLES` times (3).

---

## 7. When a run goes wrong

Fixtures stack and each owns its own cleanup, so a failing test still
untaints its nodes and deletes its ServiceGroups. A run you kill hard
does not — re-run `reset-test-cluster.sh` before the next one.

Three harness behaviours shape what a failure looks like:

- A failed metrics scrape **raises**. It is never reported as "the
  counter did not move".
- Counter assertions take a baseline and assert on the **delta**, so a
  pre-existing nonzero count from an earlier test cannot make a later one
  pass or fail.
- Log assertions are scoped to a window opened at the start of the test,
  so they cannot match a line emitted by an earlier test on another node.
