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
# Spike 11: D7 aggregate mode. The routing D7's aggregate mode relies on,
# tested through PureLB's existing remote (LB) path, since the egress
# datapath does not exist yet: every node advertises a pool's aggregate
# (v4 /24 and v6 /64, from `aggregation` left at default) through k8gobgp;
# the router ECMPs across them; a gateway that leaves -- gracefully (reboot)
# or uncleanly (its uplink stops passing frames to and from the router) -- is
# withdrawn and traffic carries on through the rest.
#
#   spike11.sh "<kubectl args>" <router-host> <probe-host> <graceful-node> <unclean-node>
#   e.g. spike11.sh "--context prox-purelb2" 172.30.255.10 172.30.250.123 purelb2-5 purelb2-4
#
# The probe host must reach the prefixes THROUGH the router, so its traffic
# is forwarded and hashed across the next hops like real replies (a probe on
# the router itself originates its connections, which are not spread the same
# way). It curls both addresses every 0.2 s; the router records its path
# count every second; every node counts the probe's packets, which shows the
# spread. The unclean failure drops every frame to or from the router's MAC
# on the node's uplink (nftables netdev ingress and egress), so BGP, BFD and
# data all stop, as on a dead link; it starts 1 s after the SSH that sets it
# returns and removes itself after 30 s.
#
#   SPIKE11_ONLY=unclean  skip the graceful reboot
#   SPIKE11_OUT=<dir>     keep the raw probe, route and event logs
#
# The prefixes are inside prox-purelb2's e2e routed bands (10.255.x,
# fc00:255:x), which the router already passes upstream for the e2e suite.
# Everything is removed at the end.
set -euo pipefail

K="kubectl $1"
RTR=$2
PROBE=$3
GNODE=$4
UNODE=$5
NS=purelb-spike11
SG=spike11-agg
SUB4=10.255.11.0/24
SUB6=fc00:255:11::/64
ONLY=${SPIKE11_ONLY:-}
OUT=${SPIKE11_OUT:-}
ROOT=$(git rev-parse --show-toplevel)
PURELB_NS=$($K get servicegroups.purelb.io -A -o jsonpath='{.items[0].metadata.namespace}')
# name and first InternalIP (dual-stack nodes list two)
mapfile -t NODES < <($K get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | awk '{print $1, $2}')
NN=${#NODES[@]} # every node advertises the aggregate
log() { echo "[$(date -u +%H:%M:%S)] $*"; }
nodeip() { local l n ip; for l in "${NODES[@]}"; do read -r n ip <<<"$l"; [ "$n" = "$1" ] && echo "$ip"; done; return 0; }
rtr() { ssh -o BatchMode=yes "$RTR" "$1"; }
prb() { ssh -o BatchMode=yes "$PROBE" "$1"; }
mark() { prb "echo \$(date +%s.%N) $* >> /tmp/spike11-events.log"; log "$*"; }
COUNTERS="table inet spike11c {
  chain pre { type filter hook prerouting priority -500; ip daddr @V4@ counter; ip6 daddr @V6@ counter; }
}"
counters() { # per-node packet counts for the two addresses
	local l
	for l in "${NODES[@]}"; do
		set -- $l
		echo "$1 $(ssh -o BatchMode=yes -o ConnectTimeout=3 "$2" 'sudo -n nft list table inet spike11c 2>/dev/null' | awk '/counter/ {for (i=1;i<=NF;i++) if ($i=="packets") s=s" "$(i+1)} END {print s}')"
	done
}

# The probe host shares a subnet with some nodes, so the router would answer
# its first packets with redirects straight to one node, bypassing ECMP (seen
# on IPv6). Real replies arrive from upstream, where that cannot happen; the
# probe host ignores redirects for the test and its settings are restored.
PIF=$(prb "ip -4 route show default | awk '{print \$5; exit}'")
REDIR="ipv4/conf/all/accept_redirects ipv4/conf/$PIF/accept_redirects ipv6/conf/all/accept_redirects ipv6/conf/$PIF/accept_redirects"
SAVED=$(prb "for f in $REDIR; do echo \$f=\$(cat /proc/sys/net/\$f); done")

cleanup() {
	log "cleanup"
	prb "for kv in $(echo $SAVED); do echo \${kv#*=} | sudo -n tee /proc/sys/net/\${kv%%=*} >/dev/null; done" || true
	prb 'pkill -f spike11-probe.s[h] 2>/dev/null; true' || true
	rtr 'pkill -f spike11-routes.s[h] 2>/dev/null; true' || true
	local l
	for l in "${NODES[@]}"; do
		set -- $l
		ssh -o BatchMode=yes -o ConnectTimeout=3 "$2" 'sudo -n nft delete table inet spike11c 2>/dev/null; sudo -n nft delete table netdev spike11 2>/dev/null; rm -f /tmp/spike11-drop.nft; true' || true
	done
	$K delete ns "$NS" --wait=true --timeout=120s >/dev/null 2>&1 || true
	$K -n "$PURELB_NS" delete servicegroups.purelb.io "$SG" --ignore-not-found >/dev/null 2>&1 || true
	sleep 10
	log "router routes left for the spike prefixes (expect none):"
	rtr "ip -4 route show $SUB4; ip -6 route show $SUB6" | sed 's/^/  /'
	prb 'rm -f /tmp/spike11-probe.sh /tmp/spike11-probe.log /tmp/spike11-events.log' || true
	rtr 'rm -f /tmp/spike11-routes.sh /tmp/spike11-routes.log' || true
}
trap cleanup EXIT

log "ServiceGroup $SG (remote, aggregation default: $SUB4 and $SUB6 from every node)"
$K apply -f - >/dev/null <<EOF
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata: {name: $SG, namespace: $PURELB_NS}
spec:
  remote:
    v4pools: [{pool: 10.255.11.100-10.255.11.110, subnet: $SUB4}]
    v6pools: [{pool: "fc00:255:11::100-fc00:255:11::110", subnet: "$SUB6"}]
EOF
$K create ns "$NS" >/dev/null
$K -n "$NS" create configmap echo-server --from-file=server.py="$ROOT/test/echo-server/server.py" >/dev/null
$K apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: spike11, namespace: $NS}
spec:
  replicas: 2
  selector: {matchLabels: {app: spike11}}
  template:
    metadata: {labels: {app: spike11}}
    spec:
      terminationGracePeriodSeconds: 3
      containers:
      - name: echo
        image: python:3.12-alpine
        command: [python3, /app/server.py]
        ports: [{containerPort: 8080}]
        volumeMounts: [{name: app, mountPath: /app}]
        readinessProbe: {tcpSocket: {port: 8080}, periodSeconds: 1}
      volumes: [{name: app, configMap: {name: echo-server}}]
---
apiVersion: v1
kind: Service
metadata:
  name: spike11
  namespace: $NS
  annotations: {purelb.io/service-group: $SG}
spec:
  type: LoadBalancer
  ipFamilyPolicy: RequireDualStack
  ipFamilies: [IPv4, IPv6]
  selector: {app: spike11}
  ports: [{port: 8080, targetPort: 8080}]
EOF
$K -n "$NS" rollout status deploy/spike11 --timeout=180s >/dev/null
for i in $(seq 1 60); do
	ips=$($K -n "$NS" get svc spike11 -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
	[ "$(echo "$ips" | wc -w)" -eq 2 ] && break
	sleep 2
done
V4=$(echo "$ips" | tr ' ' '\n' | grep -v :)
V6=$(echo "$ips" | tr ' ' '\n' | grep :)
COUNTERS=${COUNTERS//@V4@/$V4}
COUNTERS=${COUNTERS//@V6@/$V6}
log "addresses: $V4 $V6; backend pods on: $($K -n "$NS" get pods -o jsonpath='{.items[*].spec.nodeName}')"
sleep 15
log "router routes (expect $NN paths each):"
rtr "ip -4 route show $SUB4; ip -6 route show $SUB6" | sed 's/^/  /'
for l in "${NODES[@]}"; do
	set -- $l
	echo "$COUNTERS" | ssh -o BatchMode=yes "$2" 'sudo -n nft -f -'
done

# path counts on the router, every second
rtr "cat > /tmp/spike11-routes.sh" <<EOF
#!/bin/sh
paths() { n=\$(ip \$1 route show \$2 | grep -c nexthop); [ "\$n" -eq 0 ] && ip \$1 route show \$2 | grep -q . && n=1; echo \$n; }
while true; do echo \$(date +%s.%N) \$(paths -4 $SUB4) \$(paths -6 $SUB6); sleep 1; done
EOF
rtr 'chmod +x /tmp/spike11-routes.sh; setsid nohup sh -c "/tmp/spike11-routes.sh > /tmp/spike11-routes.log" </dev/null >/dev/null 2>&1 &'
prb "for f in $REDIR; do echo 0 | sudo -n tee /proc/sys/net/\$f >/dev/null; done; sudo -n ip -4 route flush cache; sudo -n ip -6 route flush cache"
# the probe, on a host behind the router
prb "cat > /tmp/spike11-probe.sh" <<EOF
#!/bin/sh
while true; do
	t=\$(date +%s.%N)
	a=\$(curl -s -o /dev/null -w '%{http_code}' -m 1 http://$V4:8080/ || true)
	b=\$(curl -s -o /dev/null -w '%{http_code}' -m 1 -g "http://[$V6]:8080/" || true)
	echo \$t \$a \$b
	sleep 0.2
done
EOF
prb 'chmod +x /tmp/spike11-probe.sh; rm -f /tmp/spike11-events.log; setsid nohup sh -c "/tmp/spike11-probe.sh > /tmp/spike11-probe.log" </dev/null >/dev/null 2>&1 &'
paths_now() { rtr "tail -1 /tmp/spike11-routes.log" | awk '{print ($2<$3)?$2:$3}'; }
sleep 30
mark "baseline-end"
log "per-node probe packets after the baseline (v4 v6):"
counters | sed 's/^/  /'
log "probe host's IPv6 route cache (expect no redirect entries):"
prb "ip -6 route show cache" | sed 's/^/  /'

if [ "$ONLY" != unclean ]; then
	# A: graceful -- reboot the node (k8gobgp stops, the session closes cleanly)
	GIP=$(nodeip "$GNODE")
	B=$(ssh -o BatchMode=yes "$GIP" cat /proc/sys/kernel/random/boot_id)
	mark "graceful-start $GNODE"
	ssh -o BatchMode=yes "$GIP" 'sudo -n systemctl reboot' || true
	for i in $(seq 1 120); do
		sleep 5
		b=$(ssh -o BatchMode=yes -o ConnectTimeout=3 "$GIP" cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
		[ -n "$b" ] && [ "$b" != "$B" ] && break
	done
	$K wait --for=condition=Ready node/"$GNODE" --timeout=600s >/dev/null
	mark "graceful-node-ready $GNODE"
	for i in $(seq 1 60); do
		[ "$(paths_now)" -ge "$NN" ] && break
		sleep 5
	done
	mark "graceful-routes-back"
	echo "$COUNTERS" | ssh -o BatchMode=yes "$GIP" 'sudo -n nft -f -' # the reboot cleared it
	sleep 20
fi

# B: unclean -- the node's uplink stops passing frames to and from the router
UIP=$(nodeip "$UNODE")
UPLINK=$(ssh -o BatchMode=yes "$UIP" "ip -4 route show default | awk '{print \$5; exit}'")
RMAC=$(ssh -o BatchMode=yes "$UIP" "ip neigh show \$(ip -4 route show default | awk '{print \$3; exit}') | awk '{print \$5}'")
log "unclean: $UNODE drops every frame to or from the router ($RMAC) on $UPLINK for 30 s"
ssh -o BatchMode=yes "$UIP" "cat > /tmp/spike11-drop.nft <<NFT
table netdev spike11 {
  chain in  { type filter hook ingress device $UPLINK priority -500; ether saddr $RMAC drop; }
  chain out { type filter hook egress device $UPLINK priority -500; ether daddr $RMAC drop; }
}
NFT
sudo -n setsid nohup sh -c 'sleep 1; nft -f /tmp/spike11-drop.nft; sleep 30; nft delete table netdev spike11' </dev/null >/dev/null 2>&1 &"
sleep 1
mark "unclean-start $UNODE"
sleep 30
mark "unclean-end $UNODE"
for i in $(seq 1 30); do
	[ "$(paths_now)" -ge "$NN" ] && break
	sleep 5
done
mark "unclean-routes-back"
sleep 20
prb 'pkill -f spike11-probe.s[h]; true'
rtr 'pkill -f spike11-routes.s[h]; true'
log "per-node probe packets at the end (v4 v6; a rebooted node's count restarts):"
counters | sed 's/^/  /'

# results: probe and events on the probe host's clock; the router's path
# counts on the router's clock (the hosts are NTP-synced; compare with care)
S=$(mktemp -d)
prb "cat /tmp/spike11-probe.log" >"$S/probe"
rtr "cat /tmp/spike11-routes.log" >"$S/routes"
prb "cat /tmp/spike11-events.log" >"$S/events"
python3 -I - "$S" <<'PY'
import sys
d = sys.argv[1]
ev = {}
for l in open(d + "/events"):
    t, n = l.split(None, 1)
    ev[n.split()[0]] = float(t)
probe = [(float(t), a, b) for t, a, b in (l.split() for l in open(d + "/probe") if len(l.split()) == 3)]
routes = [(float(t), int(a), int(b)) for t, a, b in (l.split() for l in open(d + "/routes") if len(l.split()) == 3)]
def window(t0, t1):
    p = [x for x in probe if t0 <= x[0] < t1]
    out = []
    for fam, i in (("v4", 1), ("v6", 2)):
        fails = [x[0] for x in p if x[i] != "200"]
        if fails:
            out.append(f"{fam}: {len(fails)} of {len(p)} probes failed, from +{fails[0]-t0:.1f}s to +{fails[-1]-t0:.1f}s")
        else:
            out.append(f"{fam}: 0 of {len(p)} failed")
    return "; ".join(out)
def withdrawn(t0):
    full = max(x[1] for x in routes)
    for x in routes:
        if x[0] >= t0 and (x[1] < full or x[2] < full):
            return f"{x[0]-t0:.1f}s"
    return "never"
print("baseline:           ", window(probe[0][0], ev["baseline-end"]))
if "graceful-start" in ev:
    print("graceful (reboot):  ", window(ev["graceful-start"], ev["graceful-routes-back"] + 20), "| path withdrawn after", withdrawn(ev["graceful-start"]))
print("unclean (30 s):     ", window(ev["unclean-start"], ev["unclean-routes-back"] + 20), "| path withdrawn after", withdrawn(ev["unclean-start"]))
PY
[ -n "$OUT" ] && mkdir -p "$OUT" && cp "$S"/probe "$S"/routes "$S"/events "$OUT"/
rm -rf "$S"
log "done"
