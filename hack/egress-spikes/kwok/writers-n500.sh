#!/usr/bin/env bash
# Spike 8: is the scoped design's claim latency the allocator's serial
# writer? Same run with 1, 4 and 16 writes in flight, recording the
# allocator's queue depth.
set -u
cd "$(dirname "$0")"
export NODES=500 NO_BUILD=1 DESIGNS=scoped SHAPES="250:2" QPSES="50:100"
for w in 1 4 16; do
	EXTRA="-alloc-writers $w" SUFFIX="-w$w" ./run-matrix.sh
done
echo WRITERS-DONE
