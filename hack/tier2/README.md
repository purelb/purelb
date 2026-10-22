# Tier 2 egress test clusters

Four small kubeadm clusters, one per CNI family, used to validate the egress
datapath on CNIs other than prox-purelb2's flannel. They are built on
Proxmox VMs that you create; these playbooks do everything after that.

| Cluster (pool and context) | CNI | kube-proxy |
|---|---|---|
| `t2-calico` | Calico, iptables dataplane (switchable to nftables) | nftables |
| `t2-calico-ebpf` | Calico, eBPF dataplane | none (Calico replaces it) |
| `t2-cilium` | Cilium, legacy host routing (switchable to eBPF host routing and netkit) | iptables |
| `t2-misc` | Canal by default; kube-router or Antrea with `-e misc_cni=...` | iptables |

All clusters run Kubernetes 1.35.9, dual-stack, with 3 schedulable nodes
(the control plane is not tainted). Versions and per-cluster address ranges
are in `group_vars/`.

## Prerequisites

1. **VMs.** Three VMs per cluster, in Proxmox pools named exactly as above,
   on the bridge that carries 172.30.250.0/24. Debian 13 with kernel 6.6 or
   newer, the QEMU guest agent installed and enabled (`agent: 1`), SSH for
   user `adamd` with passwordless sudo. Addresses come from DHCP.
2. **Proxmox API token.** An API token that can only see the test-cluster
   pools. The commands are in the egress plan; the role needs `VM.Audit
   VM.PowerMgmt VM.GuestAgent.Audit Pool.Audit` (plus `VM.Snapshot
   VM.Snapshot.Rollback` on the t2 pools if you want rollbacks).
   Put the credentials in `~/.config/purelb-tier2/proxmox.env` (mode 0600):

   ```
   PM_API_URL=https://<proxmox-host>:8006/api2/json
   PM_API_TOKEN_ID=claude@pve!tier2
   PM_API_TOKEN_SECRET=<secret>
   PM_CA_CERT=/home/<you>/.config/purelb-tier2/pve-ca.pem
   ```

   and copy `/etc/pve/pve-root-ca.pem` from the Proxmox host to `PM_CA_CERT`.
3. **Workstation tools.** `ansible` (core 2.19+, with `community.general`),
   `helm`, `kubectl`.

## Use

```bash
cd hack/tier2
./power.sh start t2-calico t2-calico-ebpf t2-cilium t2-misc
ansible-inventory --graph            # hosts come from the guest agent
ansible-playbook site.yml            # all clusters; or --limit t2_calico
export KUBECONFIG=~/.kube/tier2/t2-calico.yaml
kubectl get nodes -o wide
./power.sh shutdown t2-calico        # ACPI shutdown when idle
```

To switch `t2-misc` to another CNI, reset it and re-run. The same reset
rebuilds any cluster (change the group):

```bash
ansible t2_misc -b -m command -a 'kubeadm reset -f'
ansible t2_misc -b -m shell -a 'umount /run/calico/cgroup /run/cilium/cgroupv2 2>/dev/null; true'   # Calico, Canal and Cilium leave cgroup2 mounts there
ansible t2_misc -b -m shell -a 'rm -rf /etc/cni/net.d/* /root/purelb-tier2 /var/lib/cni /var/lib/calico /var/run/calico /var/run/cilium /var/lib/kube-router && { ! command -v nft >/dev/null || nft flush ruleset; }'   # the Cilium nodes have no nft
ansible t2_misc -b -m reboot    # clears the old CNI's tunnel devices, routes and veths
ansible-playbook site.yml --limit t2_misc -e misc_cni=antrea   # or kube-router, canal
```

Build options for direct routing with pod addresses outside the Node
podCIDRs. Build in the mode you want instead of switching a running cluster:
turning on Calico BGP through the `Installation` deadlocks the rollout (the
first BIRD node fails its startup probe until its peers run BIRD), and Cilium
does not support changing IPAM mode in place.

```bash
ansible-playbook site.yml --limit t2_calico -e calico_bgp=Enabled -e calico_encap=None   # BIRD mesh on 179; no k8gobgp here
ansible-playbook site.yml --limit t2_cilium -e cilium_routing=native -e cilium_ipam=cluster-pool
```

The reboot matters: Canal leaves `flannel.1`/`flannel-v6.1` with routes to the
other nodes' pod subnets, which conflict with the next CNI's routes for the
same pod CIDR.

## What it guarantees

- **Only the four t2 pools are ever touched.** The dynamic inventory
  (`inventory/proxmox.py`) refuses prox-purelb2's pool, and `power.sh` accepts
  a fixed list of pools.
- **Hostnames come from the pool and the VM's rank by VMID** (`t2-calico-1`,
  `-2`, `-3`), so a misnamed VM cannot produce duplicate node names.
- **Calico's and Canal's BGP stays off, and kube-router's BGP moves to port
  1790,** so k8gobgp keeps port 179 on every node.
- **Kubeconfigs go to `~/.kube/tier2/<cluster>.yaml`,** with the context
  named after the cluster. Your main kubeconfig is not modified.

## Networking facts these clusters rely on

- FRR at 172.30.250.1 / 2001:470:b8f3:250::1 is the gateway (AS 64514). It
  accepts the nodes through its existing `prox-purelb2` peer group, so the
  nodes peer as AS 64515. These playbooks do not change FRR.
- Since 2026-10-19 FRR runs strict `rp_filter` on `ens19`/`ens20` and hashes
  ECMP on L4 ports (`/etc/sysctl.d/99-purelb-tests.conf` on the FRR host), so
  IPv4 strict-uRPF and reply-spreading tests can run. Linux has no IPv6
  `rp_filter`.
- PureLB pool ranges per cluster are in `group_vars/` and avoid the Kea pool
  (.100–.200) and prox-purelb2's e2e ranges.
