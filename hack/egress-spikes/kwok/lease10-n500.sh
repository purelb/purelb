#!/usr/bin/env bash
# Spike 8 with PureLB's shipped 10 s lease (renewed every 5 s), N=500, 500
# addresses, QPS 50/100: baseline, decided, scoped with 1 and 4 writers, and
# scoped with 4 writers plus the allocator priority lane.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2" QPSES="50:100"
DESIGNS=none    SUFFIX=-l10 ./run-matrix.sh
DESIGNS=decided SUFFIX=-l10 ./run-matrix.sh
DESIGNS=scoped  SUFFIX=-w1-l10  ./run-matrix.sh
DESIGNS=scoped  SUFFIX=-w4-l10  EXTRA="-alloc-writers 4" ./run-matrix.sh
DESIGNS=scoped  SUFFIX=-w4p-l10 EXTRA="-alloc-writers 4 -alloc-priority" ./run-matrix.sh
echo LEASE10-DONE
