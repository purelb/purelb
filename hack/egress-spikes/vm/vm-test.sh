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
# Run the Phase 0 egress spikes on each kernel of the matrix, each in a
# throwaway QEMU VM (hack/bpf-vm/run.sh), with the egress module set.
#
#   hack/egress-spikes/vm/vm-test.sh [-run REGEX]
#
# Load and pinning spikes run with only CAP_BPF + CAP_NET_ADMIN, the agent's set.
# Namespace spikes add CAP_SYS_ADMIN (to create namespaces) and CAP_NET_RAW;
# CAP_SYS_ADMIN also counts as CAP_PERFMON to the verifier, so those runs
# prove behaviour, not loadability.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
VM=hack/bpf-vm
KERNELS=${BPF_VM_KERNELS:-debian-6.6.15 ubuntu-6.8.0-146-generic debian-6.12.111 ubuntu-6.17.0-1022-azure debian-6.18.12-bpo}
RUN=${2:-}
[ "${1:-}" = "-run" ] || RUN=

W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
LOGS=${BPF_VM_LOGS:-$(mktemp -d -t egress-spikes-vm-logs.XXXXXX)}
mkdir -p "$LOGS" "$W/files"
CGO_ENABLED=0 go build -o "$W/vminit" ./$VM/vminit
(cd hack/egress-spikes/vm && CGO_ENABLED=0 go test -c -o "$W/files/spikes.test" "./${SPIKES_PKG:-.}") # SPIKES_PKG: package dir under vm/, e.g. natspike
{
	echo "bpf,net_admin /t/spikes.test -test.v -test.run ^TestSpike(2Load|4Pin)"
	if [ -n "$RUN" ]; then
		echo "bpf,net_admin,sys_admin,net_raw /t/spikes.test -test.v -test.skip ^TestSpike(2Load|4Pin) -test.run $RUN"
	else
		echo "bpf,net_admin,sys_admin,net_raw /t/spikes.test -test.v -test.skip ^TestSpike(2Load|4Pin)"
	fi
} >"$W/cmds"

failed=()
for k in $KERNELS; do
	kdir=$(BPF_VM_MODULES=egress $VM/kernels.sh "$k")
	log="$LOGS/$k.log"
	echo "=== $k"
	result=PASS
	if ! BPF_VM_TIMEOUT=${BPF_VM_TIMEOUT:-1200} $VM/run.sh "$kdir" "$W/vminit" "$W/files" "$W/cmds" >"$log" 2>&1; then
		result=FAIL
	fi
	grep -E '^vminit: kernel|SPIKE[0-9]|^\s*--- (PASS|FAIL|SKIP)|^VMTEST ' "$log" | sed 's/^/    /'
	echo "    $result"
	if [ $result = FAIL ]; then
		failed+=("$k")
		echo "    full console: $log"
	fi
done
echo "logs: $LOGS"
if [ ${#failed[@]} -gt 0 ]; then
	echo "egress spikes: FAILED on ${failed[*]}"
	exit 1
fi
echo "egress spikes: passed on $KERNELS"
