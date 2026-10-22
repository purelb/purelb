#!/usr/bin/env bash
# Spike 8 N=500 follow-up (16 vCPU): egress-free baseline, then the D6 ack interval.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 SHAPES="250:2"
DESIGNS=none    QPSES="5:10 50:100" ./run-matrix.sh
DESIGNS=decided QPSES="50:100"      ./run-matrix.sh
DESIGNS=decided QPSES="50:100" EXTRA="-ack-interval 2s" SUFFIX="-ack2s" ./run-matrix.sh
DESIGNS=decided QPSES="50:100" EXTRA="-ack-interval 5s" SUFFIX="-ack5s" ./run-matrix.sh
echo FOLLOWUP-DONE
