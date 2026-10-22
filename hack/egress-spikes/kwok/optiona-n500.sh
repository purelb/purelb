#!/usr/bin/env bash
# Option A (D7 host-route advertisers in EgressNodeState.spec.advertisers):
# the scoped design with the decided allocator settings, N=500, 500
# addresses, QPS 50/100, 10 s lease, one fresh cluster per run.
#   none: same-day egress-free baseline (for the apiserver-cores budget)
#   r0:   scoped, no host-route addresses (same-day control)
#   r1:   100% host-route, selector = 10 nodes that run no selected pods
#   r2:   100% host-route, selector = 10 nodes that run pods like any other
#   r3:   as r2 with selector = 50 nodes
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DEC="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4"
DESIGNS=none   SUFFIX=-optA-none ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optA-r0 EXTRA="$DEC" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optA-r1-sel10-nopods EXTRA="$DEC -hostroute-frac 1 -selector-nodes 10 -selector-nopods" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optA-r2-sel10 EXTRA="$DEC -hostroute-frac 1 -selector-nodes 10" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optA-r3-sel50 EXTRA="$DEC -hostroute-frac 1 -selector-nodes 50" ./run-matrix.sh
echo OPTIONA-DONE
