#!/usr/bin/env python3
# Copyright 2026 Acnodal Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Ansible dynamic inventory for the Tier 2 egress test clusters.

Reads the Proxmox pools named in TIER2_POOLS (default: the four t2 pools),
and asks each running VM's guest agent for its addresses. Node IPs are Kea
DHCP leases, so they are discovered on every run rather than written down.

Safety: only the pools in TIER2_POOLS are ever listed. prox-purelb2's pool
is deliberately absent from the default and is refused even if named, so
these playbooks can never reach the Tier 1 cluster.

Hostnames come from the pool name plus the VM's rank by VMID within the
pool (t2-calico-1, -2, -3), not from the Proxmox VM name, so a mistyped or
duplicated VM name cannot produce duplicate Kubernetes node names.

Credentials: ~/.config/purelb-tier2/proxmox.env (PM_API_URL,
PM_API_TOKEN_ID, PM_API_TOKEN_SECRET, PM_CA_CERT). The secret is never
printed.
"""

import ipaddress
import json
import os
import ssl
import sys
import urllib.request

DEFAULT_POOLS = "t2-calico,t2-calico-ebpf,t2-cilium,t2-misc"
REFUSED_POOLS = {"purelb-cluster2"}
ENV_FILE = os.path.expanduser("~/.config/purelb-tier2/proxmox.env")


def load_env(path):
    env = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip()
    for k in ("PM_API_URL", "PM_API_TOKEN_ID", "PM_API_TOKEN_SECRET", "PM_CA_CERT"):
        if not env.get(k):
            sys.exit(f"proxmox inventory: {k} missing from {path}")
    return env


class API:
    def __init__(self, env):
        self.base = env["PM_API_URL"].rstrip("/")
        self.auth = f'PVEAPIToken={env["PM_API_TOKEN_ID"]}={env["PM_API_TOKEN_SECRET"]}'
        self.ctx = ssl.create_default_context(cafile=env["PM_CA_CERT"])
        # Python 3.13 turns on VERIFY_X509_STRICT by default, which rejects
        # the Proxmox root CA (it has no keyUsage extension). The chain and
        # hostname are still verified against PM_CA_CERT; only the strict
        # RFC 5280 extension checks are relaxed.
        self.ctx.verify_flags &= ~ssl.VERIFY_X509_STRICT

    def get(self, path):
        req = urllib.request.Request(self.base + path, headers={"Authorization": self.auth})
        try:
            with urllib.request.urlopen(req, context=self.ctx, timeout=20) as r:
                return json.load(r)["data"]
        except urllib.error.HTTPError as e:
            # The guest agent answers 500 while a VM is still booting.
            if e.code in (500, 595):
                return None
            raise


def addresses(ifaces):
    """Pick the node's IPv4 address and its stable global v6.

    The first interface carrying a non-loopback, non-link-local IPv4 address
    is the node interface.
    For IPv6, prefer the EUI-64 SLAAC address (stable across reboots) over
    privacy/temporary addresses.
    """
    for i in ifaces or []:
        name = i.get("name", "")
        if name == "lo" or name.startswith(("cni", "flannel", "veth", "kube-", "cali", "vxlan", "cilium", "lxc", "docker", "antrea", "tunl", "purelb")):
            continue
        v4 = [a["ip-address"] for a in i.get("ip-addresses", [])
              if a.get("ip-address-type") == "ipv4"
              and not ipaddress.ip_address(a["ip-address"]).is_loopback
              and not ipaddress.ip_address(a["ip-address"]).is_link_local]
        if not v4:
            continue
        v6 = [a["ip-address"] for a in i.get("ip-addresses", [])
              if a.get("ip-address-type") == "ipv6" and ipaddress.ip_address(a["ip-address"]).is_global]
        eui64 = [a for a in v6 if "ff:fe" in a]
        return name, v4[0], (eui64 or v6 or [None])[0]
    return None, None, None


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "--host":
        print("{}")
        return
    pools = [p.strip() for p in os.environ.get("TIER2_POOLS", DEFAULT_POOLS).split(",") if p.strip()]
    refused = REFUSED_POOLS.intersection(pools)
    if refused:
        sys.exit(f"proxmox inventory: refusing to manage pool(s) {sorted(refused)}")
    api = API(load_env(ENV_FILE))
    inv = {"_meta": {"hostvars": {}}, "tier2": {"children": []}}
    for pool in pools:
        group = pool.replace("-", "_")
        inv["tier2"]["children"].append(group)
        inv[group] = {"hosts": [], "vars": {"cluster_name": pool}}
        members = sorted((m for m in api.get(f"/pools/{pool}")["members"] if m.get("type") == "qemu"),
                         key=lambda m: m["vmid"])
        for rank, m in enumerate(members, start=1):
            host = f"{pool}-{rank}"
            if m.get("status") != "running":
                print(f"proxmox inventory: {host} (vmid {m['vmid']}) is {m.get('status')}; skipped", file=sys.stderr)
                continue
            agent = api.get(f"/nodes/{m['node']}/qemu/{m['vmid']}/agent/network-get-interfaces")
            iface, v4, v6 = addresses((agent or {}).get("result"))
            if not v4:
                print(f"proxmox inventory: {host} (vmid {m['vmid']}) has no guest-agent address yet; skipped", file=sys.stderr)
                continue
            inv[group]["hosts"].append(host)
            inv["_meta"]["hostvars"][host] = {
                "ansible_host": v4,
                "node_ipv4": v4,
                "node_ipv6": v6,
                "node_iface": iface,
                "vmid": m["vmid"],
                "pve_node": m["node"],
                "pve_name": m.get("name"),
                "control_plane": rank == 1,
            }
    json.dump(inv, sys.stdout, indent=2)


if __name__ == "__main__":
    main()
