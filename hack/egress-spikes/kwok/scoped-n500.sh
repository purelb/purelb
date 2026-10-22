#!/usr/bin/env bash
# Spike 8: the scoped design (acks and slice tables scoped to the nodes
# involved), at N=100 as a check and at N=500 against the baseline and the
# decided design (followup-n500.sh).
set -u
cd "$(dirname "$0")"
export NO_BUILD=1
NODES=100 DESIGNS=scoped SHAPES="50:2" QPSES="50:100" ./run-matrix.sh
NODES=500 DESIGNS=scoped SHAPES="250:2 50:2" QPSES="50:100" ./run-matrix.sh
echo SCOPED-DONE
