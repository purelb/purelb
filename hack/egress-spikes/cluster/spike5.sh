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
# Spike 5: does the chained-plugin entry survive the primary CNI rewriting
# its conflist, and a node reboot, with the purelb-cni DaemonSet prototype?
#
#   hack/egress-spikes/cluster/spike5.sh "<kubectl args>" <cni-ds-ns/name> <reboot-node>
#   e.g. spike5.sh "--context prox-purelb2" kube-flannel/kube-flannel-ds purelb2-5
#
# With SPIKE5_BOOT_PODS=<n> it also pins n restartPolicy=Always pods to the
# reboot node before the reboot, so the kubelet creates their sandboxes again
# in the boot window, racing the CNI and the installer.
#
# SPIKE5_WAR="<N> <window> <revert>" runs the installer's write-war detection
# (WAR_N, WAR_WINDOW, WAR_REVERT; item 5) and prints its whole log for the
# node; SPIKE5_PRE_SECONDS waits that long (steady state, no CNI restart)
# before restarting the CNI; SPIKE5_NO_REBOOT=1 skips the reboot step.
#
# Leaves nothing behind: the DaemonSet, the chain entry, the plugin binary,
# the spike state files and the imported image are all removed at the end.
set -euo pipefail

K="kubectl $1"
CNIDS=$2
RNODE=$3
BOOTPODS=${SPIKE5_BOOT_PODS:-0}
HERE=$(git rev-parse --show-toplevel)/hack/egress-spikes
S=$(mktemp -d)
trap 'rm -rf "$S"' EXIT
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

(cd "$HERE/vm" && CGO_ENABLED=0 go build -o "$S/purelb-cni-spike" ./cmd/purelb-cni-spike)
# Build in a private context, so concurrent runs never share files.
mkdir "$S/ctx" && cp "$S/purelb-cni-spike" "$HERE/vm/cmd/purelb-cni-spike/Dockerfile" "$S/ctx/"
docker build -q -t purelb-cni-spike:dev "$S/ctx" >/dev/null
docker save purelb-cni-spike:dev -o "$S/img.tar"

# name and first InternalIP (dual-stack nodes list two)
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
# The conflist the runtime loads (lexically first), as the installer picks it.
# /etc/cni/net.d can be root-only, so everything here reads it with sudo.
PRIMARY='sudo -n sh -c "ls /etc/cni/net.d/*.conflist /etc/cni/net.d/*.conf /etc/cni/net.d/*.json 2>/dev/null | sort | head -1"'

# Keep each node's primary conflist byte for byte, so cleanup can put back the
# original formatting (the prototype's uninstall rewrites the file).
for l in "${NODES[@]}"; do
	set -- $l
	f=$(ssh -o BatchMode=yes "$2" "$PRIMARY")
	echo "$f" >"$S/$1.path"
	ssh -o BatchMode=yes "$2" "sudo -n cat $f" >"$S/$1.conflist"
done

cleanup_cluster() {
	log "cleanup: test pod, DaemonSet, chain entry, binary, state, image"
	$K -n default delete pod spike5-a --wait=false >/dev/null 2>&1 || true
	for i in $(seq 1 "$BOOTPODS"); do $K -n default delete pod spike5-boot-$i --wait=false >/dev/null 2>&1 || true; done
	$K delete ns purelb-spike --wait=true --timeout=120s >/dev/null 2>&1 || true
	for l in "${NODES[@]}"; do
		set -- $l
		scp -q -o BatchMode=yes "$S/purelb-cni-spike" "$S/$1.conflist" "$2:/tmp/" || continue
		f=$(cat "$S/$1.path")
		ssh -o BatchMode=yes "$2" "sudo -n env HOST_ROOT= /tmp/purelb-cni-spike uninstall >/dev/null 2>&1; \
			if sudo -n python3 -c 'import json,sys; sys.exit(json.load(open(sys.argv[1])) != json.load(open(sys.argv[2])))' $f /tmp/$1.conflist; then \
				sudo -n install -m 644 /tmp/$1.conflist $(dirname $f)/.purelb-restore && sudo -n mv $(dirname $f)/.purelb-restore $f && echo '  $1 original conflist bytes restored'; \
			else echo '  $1 conflist content differs from before the spike: left as is'; fi; \
			sudo -n rm -f /tmp/purelb-cni-spike /tmp/$1.conflist /var/lib/purelb/spike5.log /var/lib/purelb/snapshot.json; \
			sudo -n rmdir /var/lib/purelb 2>/dev/null; \
			sudo -n ctr -n k8s.io images rm docker.io/library/purelb-cni-spike:dev >/dev/null 2>&1; \
			echo '  $1 entries left: '\$(sudo -n grep -c purelb-egress-spike $f)" || true
	done
}
trap 'cleanup_cluster; rm -rf "$S"' EXIT

log "importing the image on ${#NODES[@]} nodes"
for l in "${NODES[@]}"; do
	set -- $l
	scp -q -o BatchMode=yes "$S/img.tar" "$2:/tmp/purelb-cni-spike.tar"
	ssh -o BatchMode=yes "$2" "sudo -n ctr -n k8s.io images import /tmp/purelb-cni-spike.tar >/dev/null && rm /tmp/purelb-cni-spike.tar"
done

$K apply -f "$HERE/vm/cmd/purelb-cni-spike/daemonset.yaml" >/dev/null
if [ -n "${SPIKE5_WAR:-}" ]; then
	read -r wn ww wr <<<"$SPIKE5_WAR"
	$K -n purelb-spike set env ds/purelb-cni-spike WAR_N="$wn" WAR_WINDOW="$ww" WAR_REVERT="$wr" >/dev/null
fi
$K -n purelb-spike rollout status ds/purelb-cni-spike --timeout=180s >/dev/null
log "DaemonSet ready; chain entry per node:"
for l in "${NODES[@]}"; do
	set -- $l
	f=$(cat "$S/$1.path")
	echo "  $1: ${f##*/} $(ssh -o BatchMode=yes "$2" "sudo -n cat $f" | python3 -I -c 'import json,sys; print([p["type"] for p in json.load(sys.stdin)["plugins"]])')"
done

# 1. A pod goes through the chain.
$K -n default run spike5-a --image=busybox:1.37 --restart=Never --overrides="{\"spec\":{\"nodeName\":\"$RNODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 3600 >/dev/null
$K -n default wait --for=condition=Ready pod/spike5-a --timeout=120s >/dev/null
log "pod spike5-a ADD logged by the plugin: $(on "$RNODE" "grep -c 'ADD .*default/spike5-a ' /var/lib/purelb/spike5.log")"
on "$RNODE" "grep 'default/spike5-a ' /var/lib/purelb/spike5.log" | sed 's/^/  /' || true

# 2. The primary CNI rewrites its conflist: how long is the entry missing?
log "restarting $CNIDS; sampling $RNODE's conflist every 0.2s for 120s"
on "$RNODE" "nohup sh -c 'end=\$((\$(date +%s)+120)); while [ \$(date +%s) -lt \$end ]; do \
	if sudo -n grep -q purelb-egress-spike $(cat "$S/$RNODE.path"); then s=1; else s=0; fi; \
	echo \$(date +%s.%N) \$s; sleep 0.2; done' >/tmp/spike5-samples 2>&1 &" </dev/null
if [ -n "${SPIKE5_PRE_SECONDS:-}" ]; then
	log "steady state (no restart) for ${SPIKE5_PRE_SECONDS}s"
	sleep "$SPIKE5_PRE_SECONDS"
fi
log "restart of $CNIDS issued"
$K -n "${CNIDS%%/*}" rollout restart ds/"${CNIDS##*/}" >/dev/null
$K -n "${CNIDS%%/*}" rollout status ds/"${CNIDS##*/}" --timeout=300s >/dev/null
log "restart of $CNIDS rolled out"
sleep 20
on "$RNODE" "awk 'BEGIN{gone=0} \$2==0 && !gone {gone=\$1} \$2==1 && gone {printf \"  entry missing for %.1fs (from %s)\\n\", \$1-gone, gone; gone=0} END{if (gone) print \"  entry still missing at the end\"}' /tmp/spike5-samples; \
	awk '\$2==0' /tmp/spike5-samples | wc -l | sed 's/^/  samples without the entry: /'; rm -f /tmp/spike5-samples"
log "installer log on $RNODE:"
$K -n purelb-spike logs "$($K -n purelb-spike get pod -l app=purelb-cni-spike --field-selector spec.nodeName="$RNODE" -o name)" | { if [ -n "${SPIKE5_WAR:-}" ]; then cat; else tail -n 4; fi; } | sed 's/^/  /'
if [ -n "${SPIKE5_NO_REBOOT:-}" ]; then
	log "done (no reboot)"
	exit 0
fi

# 3. Reboot: which pods start before the entry is back?
for i in $(seq 1 "$BOOTPODS"); do
	$K -n default run spike5-boot-$i --image=busybox:1.37 --restart=Always --overrides="{\"spec\":{\"nodeName\":\"$RNODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 3600 >/dev/null
done
for i in $(seq 1 "$BOOTPODS"); do $K -n default wait --for=condition=Ready pod/spike5-boot-$i --timeout=120s >/dev/null; done
[ "$BOOTPODS" -gt 0 ] && log "$BOOTPODS boot-window pods Running on $RNODE"
log "rebooting $RNODE"
BOOT_BEFORE=$(on "$RNODE" "cat /proc/sys/kernel/random/boot_id")
on "$RNODE" "sudo -n systemctl reboot" || true
for i in $(seq 1 120); do
	sleep 5
	b=$(on "$RNODE" "cat /proc/sys/kernel/random/boot_id" 2>/dev/null || true)
	[ -n "$b" ] && [ "$b" != "$BOOT_BEFORE" ] && break
done
$K wait --for=condition=Ready node/"$RNODE" --timeout=600s >/dev/null
$K -n purelb-spike rollout status ds/purelb-cni-spike --timeout=300s >/dev/null
sleep 30
BOOT=$(on "$RNODE" "date -u -d \"\$(uptime -s)\" +%Y-%m-%dT%H:%M:%S")
log "$RNODE booted at $BOOT (UTC); snapshot persisted: $(on "$RNODE" "test -s /var/lib/purelb/snapshot.json && echo yes || echo no")"
log "installer start after boot:"
$K -n purelb-spike logs "$($K -n purelb-spike get pod -l app=purelb-cni-spike --field-selector spec.nodeName="$RNODE" -o name)" | sed -n 1,2p | sed 's/^/  /'
log "pod sandboxes on $RNODE since boot (UTC): attempts, first attempt, the one that succeeded, and the plugin's ADDs:"
# From containerd's own log (every node has it; crictl is not always installed).
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
$K -n default delete pod spike5-a --wait=false >/dev/null 2>&1 || true
log "done"
