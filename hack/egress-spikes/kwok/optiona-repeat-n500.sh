#!/usr/bin/env bash
# Option A repeats (same settings as optiona-n500.sh): R0 again, R0 with a
# loadgen built without the option A code paths (./loadgen-noopta, to tell a
# code effect from run-to-run variation), and R2 again.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DEC="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4"
DESIGNS=scoped SUFFIX=-optA-r0b EXTRA="$DEC" ./run-matrix.sh
cp loadgen loadgen-optA
cp loadgen-noopta loadgen
DESIGNS=scoped SUFFIX=-optA-r0-noopta EXTRA="$DEC" ./run-matrix.sh
cp loadgen-optA loadgen
DESIGNS=scoped SUFFIX=-optA-r2b-sel10 EXTRA="$DEC -hostroute-frac 1 -selector-nodes 10" ./run-matrix.sh
echo OPTIONA-REPEAT-DONE
