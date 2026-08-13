# PureLB E2E Tests

The suite is pytest, and it lives in [py/](py/). 

```bash
cd py
python3 -m venv .venv && .venv/bin/pip install -e .
../../../scripts/reset-test-cluster.sh --context <ctx> --yes
.venv/bin/pytest --context <ctx> [--router-host <frr-host>]
```

**[py/README.md](py/README.md) is the guide**: what each of the 18 test
modules covers, what a cluster must provide for them to run, and how to
run a subset.

## The cluster the suite needs

Nothing here is invented for the documentation: the fixtures discover the
topology at runtime ([purelb_e2e/topology.py](py/purelb_e2e/topology.py))
and gate tests on what they find ([conftest.py](py/conftest.py)), so this
is that logic stated forwards.

```
   workstation (this repo, .venv, kubectl)
      │
      │  kubectl --context <ctx>              → apiserver
      │  ssh <node InternalIP>                → ip addr show, curl
      │  http://<node InternalIP>:7472        → lbnodeagent metrics
      │  curl http://<VIP>                    → end-to-end, via the router
      ▼
 ┌──────────────────────────────────────────────────────────┐
 │  FRR router                                --router-host  │
 │  BGP AS 64514 · maximum-paths 8 · vtysh · tcpdump · sudo  │
 └────────┬──────────────────────────────────────┬──────────┘
          │                                      │
   subnet A 172.30.250.0/24              subnet B 172.30.251.0/24
            2001:db8:250::/64                     2001:db8:251::/64
          │                                      │
 ┌────────┴─────────┐                   ┌────────┴─────────┐
 │ node-1    node-2 │                   │ node-3    node-4 │
 └──────────────────┘                   └────────┬─────────┘
          ▲                                      │
          └──────── eth1, second NIC ────────────┘
                    on node-3, into subnet A

 every node:  gobgpd peering with the router · kube-lb0 dummy
              arp_ignore=1, arp_announce=2 · sshd · ip, curl
 free on the wire, per subnet:  .200-.253  and  <prefix>:a:: … :e::
```

The addresses above are the shape, not literals — the fixtures build
every pool from the subnets they discover.

### What each capability needs, and what you lose without it

| Capability | Configure | Without it |
|---|---|---|
| `multi-node` | 2+ nodes, all schedulable — the failover tests apply and remove `NoExecute` taints | failover, election, ECMP and stress tests skip |
| `multi-subnet` | node InternalIPs spanning 2+ /24s | subnet placement, subnet-aware election, `balancePools` and `multiPool` skip |
| `ipv6` | a **global /64** on the same interface that carries the node's InternalIP. A /128 is accepted as a fallback but yields a pool nothing can route to | the IPv6 half of every module skips |
| `dual-homed` | one node with a second NIC **on a different subnet**, named `eth*`. A second NIC on the same subnet exercises none of the discovery logic and does not count | the multi-interface and `selector_state` tests skip |
| `router` | FRR peered with every node, reachable by SSH, with `vtysh`, `tcpdump` and passwordless `sudo`. `maximum-paths 8` or there is no ECMP to assert | BGP route and on-the-wire GARP/NA tests skip |

### Node configuration

- **Free addresses.** Reserve `.200-.253` in every node subnet, and the
  `:a::` through `:e::` sub-prefixes of every node /64. The fixtures carve
  their pools from those bands, and an address handed out by DHCP or held
  by another host inside them fails in a way that reads as a PureLB bug.
- **ARP.** `net.ipv4.conf.all.arp_ignore=1` and
  `net.ipv4.conf.all.arp_announce=2`, per
  [the installation prerequisites](../../website/content/docs/installation/prerequisites/_index.md).
  Without them every node may answer ARP for a local VIP, which the
  announcing-node assertions read as the wrong node winning.
- **SSH.** Key-based, non-interactive (`BatchMode=yes`) from this
  workstation to every node InternalIP, as the invoking user. Most
  assertions read `ip -br addr show` on the node; several `curl` the VIP
  from a node. Host keys are accepted on first use.
- **Metrics.** `:7472` reachable from the workstation on every node
  (lbnodeagent is hostNetwork). The allocator is scraped through the
  apiserver proxy and needs no route.
- **Routing to the VIPs.** For the one end-to-end test, this workstation
  must reach the VIP subnets through the router — it follows the route
  BGP advertised, with no special configuration of its own.

### In the cluster

PureLB v0.17.0+ installed in `purelb-system`, and an nginx backend in
namespace `test` — `scripts/reset-test-cluster.sh` applies
[nginx-test.yaml](nginx-test.yaml) if it is missing.

## What else is here

| Path | |
|------|--|
| [nginx-test.yaml](nginx-test.yaml) | the backend every module exercises, in namespace `test`. The suite does not create it; `scripts/reset-test-cluster.sh` applies it if it is missing |
| [py/EXTERNAL-IPAM.md](py/EXTERNAL-IPAM.md) | the external-IPAM module, and how to use it as the acceptance test for your own sidecar |
