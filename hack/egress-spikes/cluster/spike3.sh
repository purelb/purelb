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
# Spike 3: does the pl_pod_tag mark survive this CNI's path into netfilter?
#
#   hack/egress-spikes/cluster/spike3.sh "<kubectl args>" <node> [seconds]
#
#   e.g. spike3.sh "--context prox-purelb2" purelb2-2
#        spike3.sh "--kubeconfig $HOME/.kube/tier2/t2-calico.yaml" t2-calico-2
#
# SPIKE3_ATTACH=netkit-peer attaches pl_pod_tag at the netkit peer hook of the
# pod's host-side primary instead of tcx ingress (netkit pods). SPIKE3_SPOOF=1
# also sends pings from addresses that are not the pod's.
#
# SPIKE3_DECIDE=<k> also reports, per family, the first observation that
# decides the classification method (mark, source IP, or neither: k tagged
# packets and none forwarded through netfilter) and how long it took.
#
# With SPIKE3_RESTART_DS=<ns>/<name> it also restarts that DaemonSet (the
# CNI) during the window, so the ORDER lines show the tcx chain on the pod's
# device before and after a CNI restart. Give it a window long enough for
# the rollout, e.g. 120 seconds.
#
# Starts one throwaway busybox pod on <node>, finds its host-side device,
# runs TestSpike3MarkOnNode there as root (SSH to the node's InternalIP,
# passwordless sudo), pings FRR from the pod in both families meanwhile, and
# removes the pod, the program and the nft table afterwards.
set -euo pipefail

K="kubectl $1"
NODE=$2
SECS=${3:-20}
POD=purelb-spike3
NS=default
GW4=${SPIKE3_GW4:-172.30.250.1}
GW6=${SPIKE3_GW6:-2001:470:b8f3:250::1}

cd "$(git rev-parse --show-toplevel)/hack/egress-spikes/vm"
BIN=$(mktemp)
OUT=$(mktemp)
cleanup() {
	$K -n $NS delete pod $POD --wait=false >/dev/null 2>&1 || true
	[ -n "${IP:-}" ] && ssh -o BatchMode=yes "$IP" "rm -f /tmp/purelb-spike3.test" >/dev/null 2>&1 || true
	rm -f "$BIN" "$OUT"
}
trap cleanup EXIT
if [ -n "${SPIKE3_BIN:-}" ]; then cp "$SPIKE3_BIN" "$BIN" && chmod 755 "$BIN"; else CGO_ENABLED=0 go test -c -o "$BIN" .; fi # SPIKE3_BIN: a prebuilt test binary

IP=$($K get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')
# SPIKE3_SPOOF=1: the pod may add addresses (NET_ADMIN), to send from one that is not its own.
CAPS=""
[ -n "${SPIKE3_SPOOF:-}" ] && CAPS=",\"containers\":[{\"name\":\"$POD\",\"image\":\"busybox:1.37\",\"args\":[\"sleep\",\"3600\"],\"securityContext\":{\"capabilities\":{\"add\":[\"NET_ADMIN\"]}}}]"
$K -n $NS run $POD --image=busybox:1.37 --restart=Never \
	--overrides="{\"spec\":{\"nodeName\":\"$NODE\",\"terminationGracePeriodSeconds\":1$CAPS}}" -- sleep 3600 >/dev/null
$K -n $NS wait --for=condition=Ready pod/$POD --timeout=120s >/dev/null
read -r P4 P6 < <($K -n $NS get pod $POD -o jsonpath='{.status.podIPs[*].ip}'; echo)
IFX=$($K -n $NS exec $POD -- cat /sys/class/net/eth0/iflink)
DEV=$(ssh -o BatchMode=yes "$IP" "ip -o link" | awk -F': ' -v i="$IFX" '$1==i {print $2}' | cut -d@ -f1)
[ -n "$DEV" ] || { echo "spike3: no host device with ifindex $IFX on $NODE" >&2; exit 1; }
echo "spike3: node=$NODE ($IP) pod=$P4 $P6 host device=$DEV"

scp -q -o BatchMode=yes "$BIN" "$IP:/tmp/purelb-spike3.test"
ssh -o BatchMode=yes "$IP" "sudo -n env SPIKE3_ATTACH=${SPIKE3_ATTACH:-} SPIKE3_DEV=$DEV SPIKE3_POD4=$P4 SPIKE3_POD6=${P6:-} SPIKE3_SECONDS=$SECS SPIKE3_DECIDE=${SPIKE3_DECIDE:-} \
	/tmp/purelb-spike3.test -test.v -test.run '^TestSpike3MarkOnNode$'" >"$OUT" 2>&1 &
SSHPID=$!
sleep 3
$K -n $NS exec $POD -- ping -c $((SECS - 6)) -i 1 "$GW4" >/dev/null 2>&1 &
$K -n $NS exec $POD -- ping -6 -c $((SECS - 6)) -i 1 "$GW6" >/dev/null 2>&1 &
if [ -n "${SPIKE3_SPOOF:-}" ]; then
	# Addresses that are not the pod's: pl_pod_tag must drop these (spoof_dropped).
	# SPIKE3_SPOOF=1 both families, 4 or 6 one family (the counter is shared).
	SP=""
	[ "$SPIKE3_SPOOF" != 6 ] && SP="ip addr add 10.99.99.99/32 dev eth0; ping -c 3 -W 1 -I 10.99.99.99 $GW4;"
	[ "$SPIKE3_SPOOF" != 4 ] && SP="$SP ip addr add fd99::99/128 dev eth0; sleep 3; ping -6 -c 3 -W 1 -I fd99::99 $GW6;"
	$K -n $NS exec $POD -- sh -c "$SP" >/dev/null 2>&1 &
	echo "spike3: sending 3 pings per family from spoofed sources (SPIKE3_SPOOF=$SPIKE3_SPOOF)"
fi
if [ -n "${SPIKE3_RESTART_DS:-}" ]; then
	echo "spike3: restarting $SPIKE3_RESTART_DS"
	$K -n "${SPIKE3_RESTART_DS%%/*}" rollout restart ds/"${SPIKE3_RESTART_DS##*/}" >/dev/null
	$K -n "${SPIKE3_RESTART_DS%%/*}" rollout status ds/"${SPIKE3_RESTART_DS##*/}" --timeout="$((SECS - 10))s" >/dev/null &&
		echo "spike3: $SPIKE3_RESTART_DS rolled out" || echo "spike3: $SPIKE3_RESTART_DS rollout NOT finished inside the window"
fi
wait $SSHPID || true
grep -E 'SPIKE3|^--- |Error|FAIL|ok' "$OUT" | grep -q SPIKE3 && grep -E 'SPIKE3|^--- |Error|FAIL|ok' "$OUT" || { echo "spike3: no result from the node; raw output:"; tail -20 "$OUT"; exit 1; }
