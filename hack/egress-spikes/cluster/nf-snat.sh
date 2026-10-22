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
# Netfilter stage end to end: does PureLB's per-slot SNAT (postrouting,
# priority 90, source-IP classification, a port range inside the slice) reach
# the wire on this CNI, for whole flows, in both families, and do replies get
# back to the pod?
#
#   hack/egress-spikes/cluster/nf-snat.sh "<kubectl args>" <node>
#
#   NF_EG4 / NF_EG6   egress addresses (from the cluster's PureLB pool range)
#   NF_UPLINK         node uplink (default eth0)
#   NF_GW4 / NF_GW6   the router's addresses (default FRR on 172.30.250.0/24)
#   NF_FRR            the router host to capture on (default 172.30.255.10)
#   NF_PORTS          the SNAT port range (default 20480-24575, the whole slice;
#                     set a part of it when the eBPF stage holds the rest)
#
# The egress addresses go on the uplink as PureLB's VIP path places them:
# IPv6 deprecated (preferred_lft 0), so the node never sources from them.
# Everything (pod, addresses, nft table, capture) is removed afterwards.
set -euo pipefail

K="kubectl $1"
NODE=$2
EG4=${NF_EG4:?set NF_EG4}
EG6=${NF_EG6:?set NF_EG6}
UPLINK=${NF_UPLINK:-eth0}
GW4=${NF_GW4:-172.30.250.1}
GW6=${NF_GW6:-2001:470:b8f3:250::1}
FRR=${NF_FRR:-172.30.255.10}
PORTS=${NF_PORTS:-20480-24575}
POD=purelb-nf-snat
CAP=/tmp/nf-snat.$(hostname -s).$$.cap # unique: other runs share the router
IP=$($K get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')
on() { ssh -n -o BatchMode=yes "$IP" "$1"; }
cleanup() {
	$K -n default delete pod $POD --wait=false >/dev/null 2>&1 || true
	on "sudo -n nft delete table inet purelb_nf_snat 2>/dev/null; sudo -n ip addr del $EG4/32 dev $UPLINK 2>/dev/null; sudo -n ip addr del $EG6/128 dev $UPLINK 2>/dev/null; true" || true
	[ -n "${CAPPID:-}" ] && ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID; sudo -n rm -f $CAP" 2>/dev/null || true
}
trap cleanup EXIT

$K -n default run $POD --image=busybox:1.37 --restart=Never \
	--overrides="{\"spec\":{\"nodeName\":\"$NODE\",\"terminationGracePeriodSeconds\":1}}" -- sleep 600 >/dev/null
$K -n default wait --for=condition=Ready pod/$POD --timeout=120s >/dev/null
read -r P4 P6 < <($K -n default get pod $POD -o jsonpath='{.status.podIPs[*].ip}'; echo)
echo "nf-snat: node=$NODE ($IP) pod=$P4 $P6 egress=$EG4 $EG6"
on "sudo -n ip addr add $EG4/32 dev $UPLINK && sudo -n ip addr add $EG6/128 dev $UPLINK nodad preferred_lft 0 && sudo -n nft -f - <<NFT
table inet purelb_nf_snat {
  chain post { type nat hook postrouting priority 90;
    ip saddr $P4 ip daddr $GW4 meta l4proto { tcp, udp } counter snat ip to $EG4:$PORTS
    ip saddr $P4 ip daddr $GW4 meta l4proto icmp counter snat ip to $EG4
    ip6 saddr $P6 ip6 daddr $GW6 meta l4proto { tcp, udp } counter snat ip6 to [$EG6]:$PORTS
    ip6 saddr $P6 ip6 daddr $GW6 meta l4proto ipv6-icmp counter snat ip6 to $EG6
  }
}
NFT"
# Only this node's frames (its uplink MAC), so concurrent runs elsewhere never mix in.
MAC=$(on "cat /sys/class/net/$UPLINK/address")
CAPPID=$(ssh -n -o BatchMode=yes "$FRR" "sudo -n sh -c 'nohup tcpdump -lnni ens19 \"ether host $MAC and (host $GW4 or host $GW6) and (icmp[0]=8 or icmp[0]=0 or (icmp6 and (ip6[40]=128 or ip6[40]=129)) or udp port 9999 or tcp port 22)\" >$CAP 2>/dev/null & echo \$!'")
sleep 2
$K -n default exec $POD -- sh -c "ping -c 2 -W 2 $GW4 | grep transmitted; ping -6 -c 2 -W 2 $GW6 | grep transmitted; \
	echo x | nc -u -w 1 $GW4 9999; echo x | nc -u -w 1 $GW6 9999; \
	(for i in 1 2 3; do echo l\$i; sleep 0.3; done) | nc -w 3 $GW4 22 | head -1; \
	(for i in 1 2 3; do echo l\$i; sleep 0.3; done) | nc -w 3 $GW6 22 | head -1" 2>&1 | sed 's/^/  pod: /' || true
echo "nf-snat: nft counters:"
on "sudo -n nft list table inet purelb_nf_snat" | grep counter | sed -E 's/^\s+/  /'
sleep 1
echo "nf-snat: sources seen at the router (towards the gateway), count per source:"
ssh -n -o BatchMode=yes "$FRR" "sudo -n kill $CAPPID; sleep 0.5; cat $CAP; sudo -n rm -f $CAP" |
	awk -v g4="$GW4" -v g6="$GW6" '
	{ for (i = 1; i <= NF; i++) if ($i == ">") { s = $(i-1); d = $(i+1); sub(/:$/, "", d) } }
	!(d == g4 || d == g6 || index(d, g4 ".") == 1 || index(d, g6 ".") == 1) { next }
	/ICMP/ { n[s]++; next }
	{ p = s; sub(/.*\./, "", p); sub(/\.[0-9]+$/, "", s); n[s]++; if (!(s in lo) || p+0 < lo[s]) lo[s] = p+0; if (p+0 > hi[s]) hi[s] = p+0 }
	END { for (s in n) printf "  %s: %d packets%s\n", s, n[s], (s in lo) ? sprintf(", ports %d-%d", lo[s], hi[s]) : "" }'
CAPPID=
echo "nf-snat: verdict: PASS if the only sources are $EG4 and $EG6 with slice ports, pings 2/2 and both SSH banners came back"
