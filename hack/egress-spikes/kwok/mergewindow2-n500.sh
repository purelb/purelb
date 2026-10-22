#!/usr/bin/env bash
# Merge window, second batch (same settings as mergewindow-n500.sh), with the
# per-lane write counters: control, -ep-merge 1s, -ep-merge 1s plus
# -spec-merge 1s, and option C (K=10, no pods on selector nodes) with
# -ep-merge 1s.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DEC="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4"
DESIGNS=scoped SUFFIX=-mw-m0c EXTRA="$DEC" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m1000c EXTRA="$DEC -ep-merge 1s" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-m1000-spec1000 EXTRA="$DEC -ep-merge 1s -spec-merge 1s" ./run-matrix.sh
DESIGNS=scoped SUFFIX=-mw-optC-k10-nopods-m1000 EXTRA="$DEC -ep-merge 1s -hostroute-model C -hostroute-frac 1 -selector-nodes 10 -selector-nopods" ./run-matrix.sh
echo MERGEWINDOW2-DONE
