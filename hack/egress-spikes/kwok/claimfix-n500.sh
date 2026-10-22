#!/usr/bin/env bash
# Spike 8: the scoped claim path after the reserve-claim fix (a claim that
# takes a reserve waits for the announcers' ack of the new owner, then the
# claimant's). N=500, 500 addresses, QPS 50/100, 10 s lease.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 DESIGNS=scoped SHAPES="250:2" QPSES="50:100"
SUFFIX=-w1-l10-fix ./run-matrix.sh
EXTRA="-alloc-writers 4 -alloc-priority" SUFFIX=-w4p-l10-fix ./run-matrix.sh
EXTRA="-alloc-writers 4 -alloc-priority -alloc-qps 200 -alloc-burst 400 -reserve 4" SUFFIX=-w4p-aq200-r4-l10-fix ./run-matrix.sh
echo CLAIMFIX-DONE
