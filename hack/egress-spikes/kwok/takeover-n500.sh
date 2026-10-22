#!/usr/bin/env bash
# Spike 8: the takeover re-read's cost (D6): the scoped design with the
# decided allocator settings, N=500, 500 addresses, 10 s lease, measuring a
# by-name GET of a slice table through a random agent's client every 250 ms.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 DESIGNS=scoped SHAPES="250:2" QPSES="50:100"
EXTRA="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4" SUFFIX=-takeover-l10 ./run-matrix.sh
echo TAKEOVER-DONE
