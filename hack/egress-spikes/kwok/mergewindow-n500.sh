#!/usr/bin/env bash
# Merge window for slice-table writes (-ep-merge): each EgressEndpoints is
# written from the ordinary queue at most once per window; claims,
# activations and the claimant's spec stay immediate. Scoped design with the
# decided allocator settings, N=500, 500 addresses, QPS 50/100, 10 s lease,
# one fresh cluster per run.
#   none:   same-day egress-free baseline
#   m0:     control, no merge
#   m250, m500, m1000: -ep-merge 250ms / 500ms / 1s
#   m1000b: repeat of m1000
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DEC="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4"
DESIGNS=none   SUFFIX=-mw-none ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m0 EXTRA="$DEC" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m250 EXTRA="$DEC -ep-merge 250ms" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m500 EXTRA="$DEC -ep-merge 500ms" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m1000 EXTRA="$DEC -ep-merge 1s" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m1000b EXTRA="$DEC -ep-merge 1s" ./run-matrix.sh
echo MERGEWINDOW-DONE
