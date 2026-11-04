#!/usr/bin/env bash
# Copyright 2026 Acnodal Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# The egress eBPF stage's load and soak tests, each kernel in a throwaway VM,
# one at a time for the load part (its CPU numbers are relative: compare within
# a kernel) and in parallel for the soak (it only reads the VM's own slab).
#
#   hack/bpf-vm/stage-perf.sh load|soak
#
#   BPF_VM_KERNELS  the kernels. The soak's default has 6.6.159 for 6.6.15, which
#                   leaks on the tunnel path by CVE-2025-40183 (egress requires it
#                   fixed); the load test keeps the kernel floor.
#   BPF_VM_LOGS     where each kernel's console log is kept
#   STAGE_SOAK      how long the soak watches each path (default 10m)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
HERE=hack/bpf-vm
PART=${1:?usage: stage-perf.sh load|soak}
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
LOGS=${BPF_VM_LOGS:-$(mktemp -d -t purelb-stage-perf.XXXXXX)}
mkdir -p "$LOGS" "$W/files"
CGO_ENABLED=0 go build -o "$W/vminit" ./$HERE/vminit
CGO_ENABLED=0 go test -c -o "$W/files/datapath.test" ./internal/egress/datapath/

case $PART in
load)
	KERNELS=${BPF_VM_KERNELS:-debian-6.6.15 ubuntu-6.8.0-146-generic debian-6.12.111 ubuntu-6.17.0-1022-azure debian-6.18.12-bpo}
	cat >"$W/cmds" <<-CMDS
		bpf,net_admin /t/datapath.test -test.v -test.run ^TestPodReplyCost\$
		bpf,net_admin,sys_admin,net_raw /t/datapath.test -test.v -test.run ^TestPerfEBPFStageLoad\$ -egress-perf
	CMDS
	;;
soak)
	KERNELS=${BPF_VM_KERNELS:-mainline-6.6.159 ubuntu-6.8.0-146-generic debian-6.12.111 ubuntu-6.17.0-1022-azure debian-6.18.12-bpo}
	cat >"$W/cmds" <<-CMDS
		bpf,net_admin,sys_admin,net_raw /t/datapath.test -test.v -test.run ^TestPerfEBPFStageSoak\$ -egress-perf -egress-soak ${STAGE_SOAK:-10m}
	CMDS
	;;
*)
	echo "stage-perf.sh: load or soak, not $PART" >&2
	exit 2
	;;
esac

one() {
	local k=$1 kdir log
	kdir=$(BPF_VM_MODULES=egress $HERE/kernels.sh "$k")
	log="$LOGS/$PART-$k.log"
	if BPF_VM_TIMEOUT=${BPF_VM_TIMEOUT:-3600} $HERE/run.sh "$kdir" "$W/vminit" "$W/files" "$W/cmds" >"$log" 2>&1; then
		echo "PASS $k"
	else
		echo "FAIL $k (full console: $log)"
		return 1
	fi
}

failed=0
if [ "$PART" = soak ]; then
	pids=()
	for k in $KERNELS; do
		one "$k" &
		pids+=($!)
	done
	for p in "${pids[@]}"; do
		wait "$p" || failed=1
	done
else
	for k in $KERNELS; do
		one "$k" || failed=1
	done
fi
grep -hE '^vminit: kernel|STAGEPERF|STAGESOAK (baseline|end|[0-9]*m0s)|pl_announce|^\s*--- (FAIL|SKIP)' "$LOGS/$PART"-*.log | sed 's/^ *//' || true
echo "logs: $LOGS"
exit $failed
