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
# Run the address guard's BPF and netns tests on each kernel of a matrix,
# each in a throwaway QEMU VM, with lbnodeagent's capabilities (CAP_BPF,
# CAP_NET_ADMIN; the netns test adds CAP_SYS_ADMIN for the namespace and
# CAP_NET_RAW for its packet socket; CAP_SYS_ADMIN also counts as
# CAP_PERFMON to the verifier, so only the first run loads the program
# exactly as the agent does). The verifier differs between kernels
# -- Linux 6.17's rejects code the others accept -- and a developer's own
# kernel tests only one of them. Run it before pushing a change to the BPF
# program or the attacher: make bpf-vm-test.
#
#   BPF_VM_KERNELS  the kernels (names as kernels.sh takes them)
#   BPF_VM_CACHE    where kernels are cached
#   BPF_VM_LOGS     where each kernel's full console log is kept
#
# Needs qemu-system-x86_64, cpio, and access to /dev/kvm (without it QEMU
# emulates, slowly). The client code must be generated (make generate).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
HERE=hack/bpf-vm
KERNELS=${BPF_VM_KERNELS:-debian-6.6.15 debian-6.12.111 debian-6.18.12-bpo ubuntu-6.8.0-146-generic ubuntu-6.17.0-1022-azure}

W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
LOGS=${BPF_VM_LOGS:-$(mktemp -d -t purelb-bpf-vm-logs.XXXXXX)}
mkdir -p "$LOGS"
mkdir -p "$W/files"
CGO_ENABLED=0 go build -o "$W/vminit" ./$HERE/vminit
CGO_ENABLED=0 go test -c -o "$W/files/addrguard.test" ./internal/addrguard/
CGO_ENABLED=0 go test -c -o "$W/files/bpfpin.test" ./internal/bpfpin/
CGO_ENABLED=0 go test -c -o "$W/files/datapath.test" ./internal/egress/datapath/
CGO_ENABLED=0 go test -c -o "$W/files/agent.test" ./internal/egress/agent/
cat >"$W/cmds" <<'CMDS'
bpf,net_admin /t/addrguard.test -test.v -test.skip ^TestAttacher
bpf,net_admin,sys_admin,net_raw /t/addrguard.test -test.v -test.run ^TestAttacher
bpf,net_admin,sys_admin /t/bpfpin.test -test.v
bpf,net_admin /t/datapath.test -test.v -test.skip ^TestNetns
bpf,net_admin,sys_admin,net_raw /t/datapath.test -test.v -test.run ^TestNetns
bpf,net_admin,sys_admin /t/agent.test -test.v -test.run ^TestNetns
CMDS

failed=()
for k in $KERNELS; do
	# The egress module set (Geneve, nf_tables, ...): a superset of the
	# base modules, which the egress tunnel tests need.
	kdir=$(BPF_VM_MODULES=egress $HERE/kernels.sh "$k")
	log="$LOGS/$k.log"
	echo "=== $k"
	result=PASS
	if ! $HERE/run.sh "$kdir" "$W/vminit" "$W/files" "$W/cmds" >"$log" 2>&1; then
		result=FAIL
	fi
	# A renamed test would otherwise pass as "no tests to run".
	for t in TestAttacherInNetns TestAttacherPinsInNetns TestPinAdoptRelease TestAdoptNothingOrStale TestSweep \
		TestLoad TestRecoverNodes TestReplySteering TestPacketTooBig TestNetnsTunnelEndToEnd TestNetnsUplinkL3 \
		TestNetnsApplyDatapath TestNetnsApplyDatapathL2; do
		if ! grep -q -- "--- PASS: $t" "$log"; then
			result=FAIL
		fi
	done
	grep -E '^vminit: (kernel|module)|^--- (FAIL|SKIP)|ns/packet|^VMTEST ' "$log" | sed 's/^/    /'
	echo "    $result"
	if [ $result = FAIL ]; then
		failed+=("$k")
		echo "    --- console (last 60 lines) ---"
		tail -60 "$log" | sed 's/^/    | /'
		echo "    full console: $log"
	fi
done

if [ ${#failed[@]} -gt 0 ]; then
	echo "bpf-vm-test: FAILED on ${failed[*]}"
	exit 1
fi
echo "bpf-vm-test: passed on $KERNELS"
