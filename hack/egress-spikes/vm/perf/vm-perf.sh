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
# Phase 0 spike 10: the egress performance measurements, on each kernel of
# the matrix, one throwaway QEMU VM at a time (hack/bpf-vm/run.sh, egress
# module set). Spike code.
#
#   hack/egress-spikes/vm/perf/vm-perf.sh
#
#   PERF_PARTS  which parts to run (default: "guard bpf natbpf net nfttx natnet";
#               also: mapmem, map memory empty and full; nftbuf, the nfttx
#               transaction with the netlink buffers raised to 1, 4, 16 MiB)
#   PERF_RUN    -test.run regex (below TestPerfNet/) for the net part
#   PERF_NATRUN -test.run regex (below TestPerfNatNet/) for the natnet part
#   BPF_VM_KERNELS, BPF_VM_LOGS, BPF_VM_TIMEOUT as for vm-test.sh
#
# guard, bpf, natbpf: BPF_PROG_TEST_RUN under CAP_BPF + CAP_NET_ADMIN only
# (the agent's set, so the verifier's hardening is in the measured code);
# guard is the address guard's own TestBPFNonVIPBudget, run in the same VM
# as the baseline the egress numbers are compared with. net, natnet: netns
# measurements, which add CAP_SYS_ADMIN and CAP_NET_RAW. nfttx is expected to
# fail (a 127-slot spike-7 nft table in one transaction exceeds google/nftables'
# default netlink socket buffers); it passes only if it fails that way.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
VM=hack/bpf-vm
KERNELS=${BPF_VM_KERNELS:-debian-6.6.15 ubuntu-6.8.0-146-generic debian-6.12.111 ubuntu-6.17.0-1022-azure debian-6.18.12-bpo}
PARTS=${PERF_PARTS:-guard bpf natbpf net nfttx natnet}
RUN=${PERF_RUN:-.}
NATRUN=${PERF_NATRUN:-.}

W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
LOGS=${BPF_VM_LOGS:-$(mktemp -d -t egress-perf-logs.XXXXXX)}
mkdir -p "$LOGS" "$W/files"
CGO_ENABLED=0 go build -o "$W/vminit" ./$VM/vminit
CGO_ENABLED=0 go test -c -o "$W/files/addrguard.test" ./internal/addrguard/
(cd hack/egress-spikes/vm && CGO_ENABLED=0 go test -c -o "$W/files/spikes.test" . &&
	CGO_ENABLED=0 go test -c -o "$W/files/natspike.test" ./natspike)
: >"$W/cmds"
for p in $PARTS; do
	case $p in
	guard) echo "bpf,net_admin /t/addrguard.test -test.v -test.run ^TestBPFNonVIPBudget$ -test.count=5" ;;
	bpf) echo "bpf,net_admin /t/spikes.test -test.v -test.run ^TestPerfBPF$ -spike10" ;;
	natbpf) echo "bpf,net_admin /t/natspike.test -test.v -test.run ^TestPerfNatBPF$ -spike10" ;;
	mapmem)
		echo "bpf,net_admin /t/spikes.test -test.v -test.run ^TestPerfMapMemory$ -spike10"
		echo "bpf,net_admin /t/natspike.test -test.v -test.run ^TestPerfMapMemory$ -spike10"
		;;
	net) echo "bpf,net_admin,sys_admin,net_raw /t/spikes.test -test.v -test.run ^TestPerfNet/$RUN -spike10" ;;
	nfttx) echo "bpf,net_admin,sys_admin,net_raw /t/spikes.test -test.v -test.run ^TestPerfNftOneTransaction$ -spike10" ;;
	nftbuf)
		# Informational: the same transaction with the netlink buffers raised.
		for b in 1048576 4194304 16777216; do
			echo "bpf,net_admin,sys_admin,net_raw /t/spikes.test -test.v -test.run ^TestPerfNftOneTransaction$ -spike10 -nftbuf=$b"
		done
		;;
	natnet) echo "bpf,net_admin,sys_admin,net_raw /t/natspike.test -test.v -test.run ^TestPerfNatNet/$NATRUN -spike10" ;;
	*) echo "vm-perf.sh: unknown part $p" >&2; exit 2 ;;
	esac
done >>"$W/cmds"

failed=()
for k in $KERNELS; do
	kdir=$(BPF_VM_MODULES=egress $VM/kernels.sh "$k")
	log="$LOGS/$k.log"
	echo "=== $k"
	BPF_VM_TIMEOUT=${BPF_VM_TIMEOUT:-3000} $VM/run.sh "$kdir" "$W/vminit" "$W/files" "$W/cmds" >"$log" 2>&1 || true
	# Every line must exit 0, except nfttx, which must fail with the
	# netlink size error.
	result=PASS
	grep -q '^VMTEST-DONE' "$log" || result=FAIL
	while read -r _ rc line; do
		case $line in
		*-nftbuf=*) ;; # informational: reported, never a failure
		*TestPerfNftOneTransaction*)
			if [ "$rc" = 0 ] || ! grep -qE 'message too long|no buffer space available' "$log"; then
				result=FAIL
			fi
			;;
		*) [ "$rc" = 0 ] || result=FAIL ;;
		esac
	done < <(grep -E '^VMTEST -?[0-9]+ ' "$log")
	grep -E '^vminit: kernel|SPIKE10|ns/packet|message too long|no buffer space|Out of memory|^\s*--- (PASS|FAIL|SKIP)|^VMTEST ' "$log" | sed 's/^/    /'
	echo "    $result"
	if [ $result = FAIL ]; then
		failed+=("$k")
		echo "    full console: $log"
	fi
done
echo "logs: $LOGS"
if [ ${#failed[@]} -gt 0 ]; then
	echo "spike 10: FAILED on ${failed[*]}"
	exit 1
fi
echo "spike 10: passed on $KERNELS"
