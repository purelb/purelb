#!/usr/bin/env bash
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
#
# Start, stop or list the VMs of a test-cluster pool through the Proxmox API.
#
#   hack/tier2/power.sh status|start|shutdown <pool>...
#
# Pools: t2-calico t2-calico-ebpf t2-cilium t2-misc t2-kwok purelb-cluster2
# ("shutdown" is an ACPI shutdown via the guest; it never force-stops).
# Credentials come from ~/.config/purelb-tier2/proxmox.env; the token secret
# is never printed.
set -euo pipefail

ENV_FILE="${HOME}/.config/purelb-tier2/proxmox.env"
ALLOWED="t2-calico t2-calico-ebpf t2-cilium t2-misc t2-kwok purelb-cluster2"

usage() { echo "usage: $0 status|start|shutdown <pool>..." >&2; exit 2; }
[[ $# -ge 2 ]] || usage
action=$1; shift
case "$action" in status|start|shutdown) ;; *) usage ;; esac

set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a

api() {
  local method=$1 path=$2
  curl -sS --fail-with-body --cacert "$PM_CA_CERT" -X "$method" \
    -H "Authorization: PVEAPIToken=${PM_API_TOKEN_ID}=${PM_API_TOKEN_SECRET}" \
    "${PM_API_URL}${path}"
}

for pool in "$@"; do
  [[ " $ALLOWED " == *" $pool "* ]] || { echo "refusing unknown pool: $pool" >&2; exit 2; }
  api GET "/pools/${pool}" | python3 -c '
import json, sys
for m in sorted(json.load(sys.stdin)["data"]["members"], key=lambda m: m["vmid"]):
    if m.get("type") == "qemu":
        print(m["node"], m["vmid"], m.get("name", "?"), m.get("status", "?"))
' | while read -r node vmid name status; do
    case "$action" in
      status)
        printf "%-16s %-5s %-20s %s\n" "$pool" "$vmid" "$name" "$status" ;;
      start)
        if [[ $status == running ]]; then
          printf "%-16s %-5s %-20s already running\n" "$pool" "$vmid" "$name"
        else
          api POST "/nodes/${node}/qemu/${vmid}/status/start" >/dev/null
          printf "%-16s %-5s %-20s start requested\n" "$pool" "$vmid" "$name"
        fi ;;
      shutdown)
        if [[ $status != running ]]; then
          printf "%-16s %-5s %-20s already %s\n" "$pool" "$vmid" "$name" "$status"
        else
          api POST "/nodes/${node}/qemu/${vmid}/status/shutdown" >/dev/null
          printf "%-16s %-5s %-20s shutdown requested\n" "$pool" "$vmid" "$name"
        fi ;;
    esac
  done
done
