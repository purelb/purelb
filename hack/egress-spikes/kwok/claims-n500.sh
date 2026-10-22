#!/usr/bin/env bash
# Spike 8: the claim path's remaining bound. Scoped design, 4 writers, the
# priority lane, the 10 s lease, N=500, 500 addresses: a higher allocator QPS,
# more reserve slices per address, and both.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 DESIGNS=scoped SHAPES="250:2" QPSES="50:100"
B="-alloc-writers 4 -alloc-priority"
EXTRA="$B -alloc-qps 200 -alloc-burst 400" SUFFIX=-w4p-aq200-l10    ./run-matrix.sh
EXTRA="$B -reserve 4"                       SUFFIX=-w4p-r4-l10       ./run-matrix.sh
EXTRA="$B -alloc-qps 200 -alloc-burst 400 -reserve 4" SUFFIX=-w4p-aq200-r4-l10 ./run-matrix.sh
echo CLAIMS-DONE
