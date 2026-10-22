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
# The eBPF pod-node stage (natspike's pl_pod_out / pl_pod_reply) on a real
# cluster node: does PureLB's own NAT reach the wire, and do replies come
# back to the pod?
#
#   hack/egress-spikes/cluster/ebpf-stage.sh "<kubectl args>" <node> <head|tail>
#   e.g. ebpf-stage.sh "--kubeconfig $HOME/.kube/tier2/t2-calico-ebpf.yaml" t2-calico-ebpf-3 head
#
# EBPF_NAT_KEY=src registers the pod by source address (netkit, where the
# packet reaches the NIC with no ingress device) instead of by device.
# EBPF_SLICE_START / EBPF_SLICE_SIZE set the slice (default 20480 / 4096; the
# size must be a power of two).
#
# Puts the egress addresses on the node's uplink, starts one busybox pod on
# <node>, runs TestSpike2NatOnNode there as root, sends ping, UDP and TCP
# (the router's SSH banner) from the pod in both families, and captures what
# reaches the router (FRR, ens19). Removes the pod, the addresses, the
# programs and the capture afterwards.
set -euo pipefail

K="kubectl $1"
NODE=$2
ANCHOR=$3
UPLINK=${EBPF_UPLINK:-eth0}
EG4=${EBPF_EG4:-172.30.250.25}
EG6=${EBPF_EG6:-2001:470:b8f3:250:11::5}
GW4=${EBPF_GW4:-172.30.250.1}
GW6=${EBPF_GW6:-2001:470:b8f3:250::1}
FRR=${EBPF_FRR:-172.30.255.10}
POD=purelb-ebpf-stage
NS=default
S=$(mktemp -d)

cd "$(git rev-parse --show-toplevel)/hack/egress-spikes/vm/natspike"
IP=$($K get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')
on() { ssh -n -o BatchMode=yes "$IP" "$1"; }
cleanup() {
	$K -n $NS delete pod $POD --wait=false >/dev/null 2>&1 || true
	# The test exits by itself after NAT_SECONDS, which detaches its programs.
	on "sudo -n ip addr del $EG4/32 dev $UPLINK 2>/dev/null; sudo -n ip addr del $EG6/128 dev $UPLINK 2>/dev/null; rm -f /tmp/purelb-ebpf-stage.test" || true
	[ -n "${CAPPID:-}" ] && ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID" 2>/dev/null || true
	rm -rf "$S"
}
trap cleanup EXIT

CGO_ENABLED=0 go test -c -o "$S/t" .
scp -q -o BatchMode=yes "$S/t" "$IP:/tmp/purelb-ebpf-stage.test"
on "sudo -n ip addr add $EG4/32 dev $UPLINK && sudo -n ip addr add $EG6/128 dev $UPLINK nodad preferred_lft 0"

$K -n $NS run $POD --image=busybox:1.37 --restart=Never \
	--overrides="{\"spec\":{\"nodeName\":\"$NODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 3600 >/dev/null
$K -n $NS wait --for=condition=Ready pod/$POD --timeout=120s >/dev/null
read -r P4 P6 < <($K -n $NS get pod $POD -o jsonpath='{.status.podIPs[*].ip}'; echo)
IFX=$($K -n $NS exec $POD -- cat /sys/class/net/eth0/iflink)
DEV=$(on "ip -o link" | awk -F': ' -v i="$IFX" '$1==i {print $2}' | cut -d@ -f1)
echo "ebpf-stage: node=$NODE ($IP) pod=$P4 $P6 dev=$DEV anchor=$ANCHOR egress=$EG4 $EG6"

# What reaches the router: echo, UDP 9999, TCP 22 to the gateway addresses.
CAPPID=$(ssh -n -o BatchMode=yes "$FRR" "sudo -n sh -c 'nohup tcpdump -lnni ens19 \"(dst host $GW4 or src host $GW4 or dst host $GW6 or src host $GW6) and (icmp[0]=8 or icmp[0]=0 or (icmp6 and (ip6[40]=128 or ip6[40]=129)) or udp port 9999 or tcp port 22)\" >/tmp/ebpf-stage.cap 2>/dev/null & echo \$!'")

ssh -n -o BatchMode=yes "$IP" "sudo -n env NAT_POD_DEV=$DEV NAT_UPLINK=$UPLINK NAT_POD4=$P4 NAT_POD6=$P6 NAT_EG4=$EG4 NAT_EG6=$EG6 NAT_ANCHOR=$ANCHOR NAT_KEY=${EBPF_NAT_KEY:-} NAT_SLICE_START=${EBPF_SLICE_START:-} NAT_SLICE_SIZE=${EBPF_SLICE_SIZE:-} NAT_SECONDS=25 \
	/tmp/purelb-ebpf-stage.test -test.v -test.run '^TestSpike2NatOnNode$'" >"$S/out" 2>&1 &
TPID=$!
for _ in $(seq 1 30); do grep -q "NAT READY" "$S/out" 2>/dev/null && break; sleep 0.5; done
grep -q "NAT READY" "$S/out" || { echo "ebpf-stage: test did not start:"; cat "$S/out"; exit 1; }

x() { echo "  pod\$ $*"; $K -n $NS exec $POD -- sh -c "$*" 2>&1 | sed 's/^/    /' || true; }
x "ping -c 2 -W 2 $GW4 | tail -2"
x "ping -6 -c 2 -W 2 $GW6 | tail -2"
x "echo purelb | nc -u -w 1 $GW4 9999; echo sent"
x "echo purelb | nc -u -w 1 $GW6 9999; echo sent"
x "nc -w 3 $GW4 22 </dev/null | head -1"
x "nc -w 3 $GW6 22 </dev/null | head -1"

wait $TPID || true
grep -E "NAT (CHAIN|RESULT|ENTRY)|FAIL|Error" "$S/out" | sed 's/^ *//'
echo "ebpf-stage: at the router (ens19):"
sleep 1
ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID; sleep 0.5; cat /tmp/ebpf-stage.cap; sudo -n rm -f /tmp/ebpf-stage.cap" | cut -c1-160 | sed 's/^/  /'
CAPPID=
