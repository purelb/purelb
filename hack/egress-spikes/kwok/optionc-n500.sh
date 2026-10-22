#!/usr/bin/env bash
# Option C (every live announcerNodeSelector node advertises every routed
# host-route egress address, watches and acks its slice table; activation
# gates on all K; no spec.advertisers): the scoped design with the decided
# allocator settings, N=500, 500 addresses, QPS 50/100, 10 s lease, one
# fresh cluster per run.
#   none: same-day egress-free baseline
#   c0:   scoped, no host-route addresses (same-day control)
#   c1:   100% host-route, K=10 selector nodes that run no selected pods
#   c2:   100% host-route, K=10, selector nodes run pods like the rest
#   c3:   100% host-route, K=50, selector nodes run pods
#   c1b:  repeat of c1
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DEC="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4"
C="-hostroute-model C -hostroute-frac 1"
DESIGNS=none   SUFFIX=-optC-none ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optC-c0 EXTRA="$DEC" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optC-c1-sel10-nopods EXTRA="$DEC $C -selector-nodes 10 -selector-nopods" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optC-c2-sel10 EXTRA="$DEC $C -selector-nodes 10" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optC-c3-sel50 EXTRA="$DEC $C -selector-nodes 50" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-optC-c1b-sel10-nopods EXTRA="$DEC $C -selector-nodes 10 -selector-nopods" ./run-matrix.sh
echo OPTIONC-DONE
