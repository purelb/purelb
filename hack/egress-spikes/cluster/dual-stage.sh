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
# Both pod-node stages armed at once for the same pods: the netfilter stage
# (nft postrouting SNAT, priority 90, into one part of the node's slice) and
# the eBPF stage (natspike pl_pod_out at the head of the uplink's tcx egress,
# pl_pod_reply at the head of its tcx ingress, into the other part). Whichever
# stage sees the pod's source translates it; the port (or echo id) on the
# wire says which one did.
#
#   hack/egress-spikes/cluster/dual-stage.sh "<kubectl args>" <node>
#
#   DS_EG4 / DS_EG6       egress addresses (required)
#   DS_UPLINK             node uplink (default eth0)
#   DS_GW4 / DS_GW6       the router's addresses (default FRR on 172.30.250.0/24)
#   DS_FRR                the router host to capture on (default 172.30.255.10)
#   DS_NPODS              pods on the node, 1 or 2 (default 1)
#   DS_ANN1 / DS_ANN2     each pod's annotations, a JSON object (e.g. Calico's
#                         cni.projectcalico.org/ipv4pools)
#   DS_MODE               dual (default) | nf | ebpf | observe (nft counters
#                         only, nothing translated) | pod (eBPF NAT at the
#                         head of the pod's host-side tcx ingress, before the
#                         CNI; no nft)
#   DS_NF_PORTS           netfilter range, ports and echo ids (default 20480-22527)
#   DS_EBPF_START / _SIZE eBPF slice (default 22528 / 2048)
#   DS_NAT_KEY            src: the eBPF stage finds the pod by source address
#   DS_AFTER              a local command run after the traffic, before cleanup,
#                         with DEVS set to the pods' host-side devices (e.g. to
#                         read the CNI's own drop counters)
#
# Pod i sends ping, UDP to port 10000-i and a multi-packet TCP exchange (the
# router's SSH port) in both families; the pods send at the same time.
# Everything (pods, addresses, nft table, programs, capture) is removed
# afterwards.
set -euo pipefail

K="kubectl $1"
NODE=$2
EG4=${DS_EG4:?set DS_EG4}
EG6=${DS_EG6:?set DS_EG6}
UPLINK=${DS_UPLINK:-eth0}
GW4=${DS_GW4:-172.30.250.1}
GW6=${DS_GW6:-2001:470:b8f3:250::1}
FRR=${DS_FRR:-172.30.255.10}
NPODS=${DS_NPODS:-1}
MODE=${DS_MODE:-dual}
NFP=${DS_NF_PORTS:-20480-22527}
ES=${DS_EBPF_START:-22528}
EZ=${DS_EBPF_SIZE:-2048}
NS=default
S=$(mktemp -d)
CAP=/tmp/dual-stage.$(hostname -s).$$.cap # unique: other runs share the router
BIN=/tmp/purelb-dual-stage.$$.test
case $MODE in dual | nf | ebpf | observe | pod) ;; *) echo "bad DS_MODE $MODE" >&2; exit 2 ;; esac
NFT=0; BPF=0
case $MODE in dual) NFT=1; BPF=1 ;; nf | observe) NFT=1 ;; ebpf | pod) BPF=1 ;; esac

IP=$($K get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')
on() { ssh -n -o BatchMode=yes "$IP" "$1"; }
cleanup() {
	for i in $(seq 1 "$NPODS"); do $K -n $NS delete pod purelb-ds-$i --wait=false >/dev/null 2>&1 || true; done
	on "sudo -n pkill -f \"^$BIN\"; sudo -n nft delete table inet purelb_dual 2>/dev/null; sudo -n ip addr del $EG4/32 dev $UPLINK 2>/dev/null; sudo -n ip addr del $EG6/128 dev $UPLINK 2>/dev/null; rm -f $BIN; true" || true
	[ -n "${CAPPID:-}" ] && ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID; sudo -n rm -f $CAP" 2>/dev/null || true
	rm -rf "$S"
}
trap cleanup EXIT

if [ $BPF = 1 ]; then
	(cd "$(git rev-parse --show-toplevel)/hack/egress-spikes/vm/natspike" && CGO_ENABLED=0 go test -c -o "$S/t" .)
	scp -q -o BatchMode=yes "$S/t" "$IP:$BIN"
fi

P4=(); P6=(); DEV=()
for i in $(seq 1 "$NPODS"); do
	ann=DS_ANN$i; a=${!ann:-}; [ -n "$a" ] || a='{}'
	$K -n $NS wait --for=delete pod/purelb-ds-$i --timeout=60s >/dev/null 2>&1 || true # a previous run's pod
	$K -n $NS run purelb-ds-$i --image=busybox:1.37 --restart=Never \
		--overrides="{\"metadata\":{\"annotations\":$a},\"spec\":{\"nodeName\":\"$NODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 3600 >/dev/null
done
for i in $(seq 1 "$NPODS"); do
	$K -n $NS wait --for=condition=Ready pod/purelb-ds-$i --timeout=120s >/dev/null
	read -r a b < <($K -n $NS get pod purelb-ds-$i -o jsonpath='{.status.podIPs[*].ip}'; echo)
	P4+=("$a"); P6+=("$b")
	ifx=$($K -n $NS exec purelb-ds-$i -- cat /sys/class/net/eth0/iflink)
	DEV+=("$(on "ip -o link" | awk -F': ' -v i="$ifx" '$1==i {print $2}' | cut -d@ -f1)")
	ann=DS_ANN$i
	echo "dual-stage: pod $i: ${P4[-1]} ${P6[-1]} dev=${DEV[-1]} annotations=${!ann:-none}"
done
echo "dual-stage: node=$NODE ($IP) mode=$MODE egress=$EG4 $EG6 nf=$NFP ebpf=$ES+$EZ"

on "sudo -n ip addr add $EG4/32 dev $UPLINK && sudo -n ip addr add $EG6/128 dev $UPLINK nodad preferred_lft 0"
if [ $NFT = 1 ]; then
	pre=; post=
	for i in $(seq 0 $((NPODS - 1))); do
		pre+="ip saddr ${P4[$i]} counter comment \"pre4 pod$((i + 1))\"
    ip6 saddr ${P6[$i]} counter comment \"pre6 pod$((i + 1))\"
    "
		[ "$MODE" = observe ] && continue
		post+="ip saddr ${P4[$i]} ip daddr $GW4 meta l4proto { tcp, udp } counter snat ip to $EG4:$NFP comment \"snat4 pod$((i + 1))\"
    ip saddr ${P4[$i]} ip daddr $GW4 meta l4proto icmp counter snat ip to $EG4:$NFP comment \"snat4icmp pod$((i + 1))\"
    ip6 saddr ${P6[$i]} ip6 daddr $GW6 meta l4proto { tcp, udp } counter snat ip6 to [$EG6]:$NFP comment \"snat6 pod$((i + 1))\"
    ip6 saddr ${P6[$i]} ip6 daddr $GW6 meta l4proto ipv6-icmp counter snat ip6 to [$EG6]:$NFP comment \"snat6icmp pod$((i + 1))\"
    "
	done
	on "sudo -n nft -f - <<NFT
table inet purelb_dual {
  chain pre { type filter hook prerouting priority -150;
    $pre
  }
  chain post { type nat hook postrouting priority 90;
    $post
  }
}
NFT"
fi

# Only this node's frames (its uplink MAC), so concurrent runs elsewhere never mix in.
MAC=$(on "cat /sys/class/net/$UPLINK/address")
CAPPID=$(ssh -n -o BatchMode=yes "$FRR" "sudo -n sh -c 'nohup tcpdump -lnni ens19 \"ether host $MAC and (host $GW4 or host $GW6) and (icmp[0]=8 or icmp[0]=0 or icmp[0]=3 or (icmp6 and (ip6[40]=128 or ip6[40]=129 or ip6[40]=1)) or udp portrange 9990-9999 or tcp port 22)\" >$CAP 2>/dev/null & echo \$!'")

if [ $BPF = 1 ]; then
	attach=; [ "$MODE" = pod ] && attach=pod
	ssh -n -o BatchMode=yes "$IP" "sudo -n env NAT_POD_DEV='${DEV[*]}' NAT_UPLINK=$UPLINK NAT_POD4='${P4[*]}' NAT_POD6='${P6[*]}' NAT_EG4=$EG4 NAT_EG6=$EG6 NAT_ANCHOR=head NAT_KEY=${DS_NAT_KEY:-} NAT_SLICE_START=$ES NAT_SLICE_SIZE=$EZ NAT_ATTACH=$attach NAT_SECONDS=30 \
		$BIN -test.v -test.run '^TestSpike2NatOnNode\$'" >"$S/out" 2>&1 &
	TPID=$!
	for _ in $(seq 1 30); do grep -q "NAT READY" "$S/out" 2>/dev/null && break; sleep 0.5; done
	grep -q "NAT READY" "$S/out" || { echo "dual-stage: eBPF stage did not start:"; cat "$S/out"; exit 1; }
fi
sleep 2

traffic() { # pod index (1-based)
	$K -n $NS exec purelb-ds-$1 -- sh -c "ping -c 2 -W 2 $GW4 | grep transmitted; ping -6 -c 2 -W 2 $GW6 | grep transmitted; \
		echo x | nc -u -w 1 $GW4 $((10000 - $1)); echo x | nc -u -w 1 $GW6 $((10000 - $1)); \
		(for i in 1 2 3; do echo l\$i; sleep 0.3; done) | nc -w 3 $GW4 22 | head -1; \
		(for i in 1 2 3; do echo l\$i; sleep 0.3; done) | nc -w 3 $GW6 22 | head -1" 2>&1 | sed "s/^/  pod$1: /" || true
}
TR=()
for i in $(seq 1 "$NPODS"); do traffic "$i" >"$S/pod$i" & TR+=($!); done
wait "${TR[@]}" || true
cat "$S"/pod*
[ -z "${DS_AFTER:-}" ] || DEVS="${DEV[*]}" bash -c "$DS_AFTER" 2>&1 | sed 's/^/  after: /'

if [ $NFT = 1 ]; then
	echo "dual-stage: nft counters (pre = reached netfilter, the A2 signal; snat = netfilter translated):"
	on "sudo -n nft list table inet purelb_dual" | grep counter | sed -E 's/^\s+/  /'
fi
if [ $BPF = 1 ]; then
	wait "$TPID" || true
	echo "dual-stage: eBPF stage:"
	grep -E "NAT (SLICE|CHAIN|RESULT|ENTRY)|FAIL|Error|panic" "$S/out" | sed 's/^ *//; s/^/  /'
fi
sleep 1
echo "dual-stage: at the router, per source and protocol (ports / echo ids; nf=$NFP, ebpf=$ES-$((ES + EZ - 1))):"
ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID; sleep 0.5; cat $CAP; sudo -n rm -f $CAP" | tee "$S/cap" |
	awk -v g4="$GW4" -v g6="$GW6" -v nfr="$NFP" -v es="$ES" -v ez="$EZ" '
	BEGIN { split(nfr, r, "-"); nlo = r[1]; nhi = r[2] }
	function half(v) { v += 0; return (v >= nlo + 0 && v <= nhi + 0) ? "nf" : (v >= es && v < es + ez) ? "ebpf" : "OTHER" }
	{ for (i = 1; i <= NF; i++) if ($i == ">") { s = $(i-1); d = $(i+1); sub(/:$/, "", d) } }
	{ out = (d == g4 || d == g6 || index(d, g4 ".") == 1 || index(d, g6 ".") == 1) }
	/echo request|echo reply/ { a = out ? s : d; v = $0; sub(/.*id /, "", v); sub(/,.*/, "", v); p = "icmp"; if (!out) p = "icmp-reply" }
	/unreachable/ { if (!out) { a = d; p = "icmp-error"; v = -1 } else next }
	/UDP|Flags/ { a = out ? s : d; o = out ? d : s; v = a; sub(/.*\./, "", v); sub(/\.[0-9]+$/, "", a)
		dp = o; sub(/.*\./, "", dp); p = (/UDP/ ? "udp" : "tcp") (out ? "" : "-reply") "->" dp
		if (!out) p = (/UDP/ ? "udp" : "tcp") "-reply" }
	{ k = a " " p; n[k]++; if (v >= 0) { h[k, half(v)]++; if (!(k SUBSEP v in seen)) { seen[k, v]; ids[k] = ids[k] " " v } } }
	END { for (k in n) printf "  %-40s %3d pkts  nf=%d ebpf=%d other=%d  values:%s\n", k, n[k], h[k, "nf"], h[k, "ebpf"], h[k, "OTHER"], ids[k] }' | sort
echo "dual-stage: sources other than $EG4 / $EG6 towards the router (should be none):"
awk -v g4="$GW4" -v g6="$GW6" -v e4="$EG4" -v e6="$EG6" '
	{ for (i = 1; i <= NF; i++) if ($i == ">") { s = $(i-1); d = $(i+1); sub(/:$/, "", d) } }
	(d == g4 || d == g6 || index(d, g4 ".") == 1 || index(d, g6 ".") == 1) && !(s == e4 || s == e6 || index(s, e4 ".") == 1 || index(s, e6 ".") == 1) { print "  " $0 }' "$S/cap" | cut -c1-150
CAPPID=
