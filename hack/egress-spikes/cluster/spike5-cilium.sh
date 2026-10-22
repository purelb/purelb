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
# Spike 5 on Cilium: the boot window with PureLB chained through Cilium's own
# CNI configuration (Helm cni.configMap, D34), not the installer DaemonSet,
# which fights Cilium over its conflist.
#
#   hack/egress-spikes/cluster/spike5-cilium.sh "<kubectl args>" <reboot-node> [boot-pods]
#   e.g. spike5-cilium.sh "--kubeconfig $HOME/.kube/tier2/t2-cilium.yaml" t2-cilium-3 5
#
# Installs only the plugin binary on every node, adds a ConfigMap holding
# Cilium's conflist with the plugin appended, points Cilium at it, pins
# <boot-pods> restartPolicy=Always pods to <reboot-node>, reboots it, and
# reports each pod sandbox created since boot and the plugin's ADDs for it.
# Rolls Cilium back to the Helm revision it started from and removes the
# ConfigMap, the binary and the plugin state.
set -euo pipefail

KARGS=$1
K="kubectl $KARGS"
H="helm ${KARGS/--context/--kube-context}"
RNODE=$2
BOOTPODS=${3:-5}
HERE=$(git rev-parse --show-toplevel)/hack/egress-spikes
S=$(mktemp -d)
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

mapfile -t NODES < <($K get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | awk '{print $1, $2}')
nodeip() {
	local want=$1 l n ip
	for l in "${NODES[@]}"; do
		read -r n ip <<<"$l"
		[ "$n" = "$want" ] && { echo "$ip"; return; }
	done
}
# -n: never read stdin, so on() is safe inside a while-read loop.
on() { ssh -n -o BatchMode=yes "$(nodeip "$1")" "$2"; }
types() { on "$1" 'sudo -n cat /etc/cni/net.d/05-cilium.conflist' | python3 -I -c 'import json,sys; print([p["type"] for p in json.load(sys.stdin)["plugins"]])'; }

REV=$($H history cilium -n kube-system -o json | python3 -I -c 'import json,sys; print([r["revision"] for r in json.load(sys.stdin) if r["status"]=="deployed"][0])')
$H get values cilium -n kube-system -o yaml >"$S/values-before.yaml"
cleanup() {
	log "cleanup: Helm rollback to revision $REV, ConfigMap, pods, binary, state"
	for i in $(seq 1 "$BOOTPODS"); do $K -n default delete pod spike5-boot-$i --wait=false >/dev/null 2>&1 || true; done
	$H rollback cilium "$REV" -n kube-system --wait --timeout 5m >/dev/null 2>&1 || echo "  helm rollback FAILED"
	$K -n kube-system rollout status ds/cilium --timeout=300s >/dev/null 2>&1 || true
	$K -n kube-system delete configmap purelb-cilium-cni --ignore-not-found >/dev/null 2>&1 || true
	for l in "${NODES[@]}"; do
		set -- $l
		ssh -n -o BatchMode=yes "$2" 'sudo -n rm -f /opt/cni/bin/purelb-egress-spike /var/lib/purelb/spike5.log; sudo -n rmdir /var/lib/purelb 2>/dev/null; true' || true
		echo "  $1: $(types "$1" 2>/dev/null || echo '?')"
	done
	$H get values cilium -n kube-system -o yaml | diff -q - "$S/values-before.yaml" >/dev/null &&
		echo "  Helm values identical to before" || echo "  Helm values DIFFER from before"
	rm -rf "$S"
}
trap cleanup EXIT

(cd "$HERE/vm" && CGO_ENABLED=0 go build -o "$S/purelb-egress-spike" ./cmd/purelb-cni-spike)
for l in "${NODES[@]}"; do
	set -- $l
	scp -q -o BatchMode=yes "$S/purelb-egress-spike" "$2:/tmp/"
	ssh -n -o BatchMode=yes "$2" 'sudo -n install -m 755 /tmp/purelb-egress-spike /opt/cni/bin/purelb-egress-spike && rm /tmp/purelb-egress-spike && sudo -n mkdir -p /var/lib/purelb'
done
CONF=$(on "$RNODE" 'sudo -n cat /etc/cni/net.d/05-cilium.conflist' | python3 -I -c 'import json,sys; c=json.load(sys.stdin); c["plugins"].append({"type":"purelb-egress-spike"}); print(json.dumps(c))')
$K -n kube-system create configmap purelb-cilium-cni --from-literal=cni-config="$CONF" >/dev/null
$H upgrade cilium cilium --repo https://helm.cilium.io --version "$($H list -n kube-system -f '^cilium$' -o json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[0]["chart"].split("-",1)[1])')" \
	-n kube-system --reuse-values --set cni.configMap=purelb-cilium-cni --wait --timeout 5m >/dev/null
$K -n kube-system rollout status ds/cilium --timeout=300s >/dev/null
log "chained through cni.configMap; conflist per node:"
for l in "${NODES[@]}"; do set -- $l; echo "  $1: $(types "$1")"; done

for i in $(seq 1 "$BOOTPODS"); do
	$K -n default run spike5-boot-$i --image=busybox:1.37 --restart=Always --overrides="{\"spec\":{\"nodeName\":\"$RNODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 3600 >/dev/null
done
for i in $(seq 1 "$BOOTPODS"); do $K -n default wait --for=condition=Ready pod/spike5-boot-$i --timeout=120s >/dev/null; done
log "$BOOTPODS boot-window pods Running on $RNODE; ADDs logged so far: $(on "$RNODE" "grep -c 'ADD .*default/spike5-boot-' /var/lib/purelb/spike5.log")"

log "rebooting $RNODE"
BOOT_BEFORE=$(on "$RNODE" "cat /proc/sys/kernel/random/boot_id")
on "$RNODE" "sudo -n systemctl reboot" || true
for i in $(seq 1 120); do
	sleep 5
	b=$(on "$RNODE" "cat /proc/sys/kernel/random/boot_id" 2>/dev/null || true)
	[ -n "$b" ] && [ "$b" != "$BOOT_BEFORE" ] && break
done
$K wait --for=condition=Ready node/"$RNODE" --timeout=600s >/dev/null
$K -n kube-system rollout status ds/cilium --timeout=300s >/dev/null
for i in $(seq 1 "$BOOTPODS"); do $K -n default wait --for=condition=Ready pod/spike5-boot-$i --timeout=300s >/dev/null || true; done
sleep 10
log "$RNODE booted at $(on "$RNODE" "date -u -d \"\$(uptime -s)\" +%Y-%m-%dT%H:%M:%S") (UTC); conflist now: $(types "$RNODE")"
log "conflist written at (cilium-agent): $(on "$RNODE" 'sudo -n stat -c %y /etc/cni/net.d/05-cilium.conflist' | cut -c12-19)"
log "pod sandboxes on $RNODE since boot (UTC): attempts, first attempt, the one that succeeded, and the plugin's ADDs:"
on "$RNODE" "sudo -n journalctl -b -u containerd --no-pager" | python3 -I -c '
import re, sys
pods = {}
for l in sys.stdin:
    m = re.search(r"time=\"[0-9-]+T([0-9:]+)\.\d+Z\".*RunPodSandbox for &PodSandboxMetadata\{Name:([^,]+),Uid:[^,]+,Namespace:([^,]+),", l)
    if not m:
        continue
    p = pods.setdefault(m.group(3) + "/" + m.group(2), {"n": 0, "first": m.group(1), "ok": "-"})
    if "returns sandbox id" in l:
        p["ok"] = m.group(1)
    else:
        p["n"] += 1
for name, p in pods.items():
    print(name, p["n"], p["first"], p["ok"])
' | while read -r name n first ok; do
	hostnet=$($K -n "${name%%/*}" get pod "${name##*/}" -o jsonpath='{.spec.hostNetwork}' 2>/dev/null || echo "?")
	adds=$(on "$RNODE" "grep 'ADD .* args=$name ' /var/lib/purelb/spike5.log | cut -c12-19 | tr '\n' ' '" || true)
	echo "  $name hostNetwork=${hostnet:-false} attempts=$n first=$first succeeded=$ok plugin_ADDs_at=[${adds% }]"
done
log "done"
