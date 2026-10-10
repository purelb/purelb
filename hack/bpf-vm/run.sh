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
# Boot one kernel in QEMU with an initramfs holding vminit (as /init), the
# files in a directory (under /t) and the kernel's modules, run the
# commands in a cmds file (see vminit), and print the console. Exits 0
# only if every command did.
#
#   run.sh <kernel dir from kernels.sh> <vminit binary> <files dir> <cmds file>
set -euo pipefail

KDIR=$1 VMINIT=$2 FILES=$3 CMDS=$4
ROOT=$(mktemp -d)
OUT=$(mktemp)
trap 'rm -rf "$ROOT" "$ROOT.initrd" "$OUT"' EXIT

mkdir -p "$ROOT/t" "$ROOT/modules"
cp "$VMINIT" "$ROOT/init"
cp "$FILES"/* "$ROOT/t/"
cp "$CMDS" "$ROOT/cmds"
if [ -d "$KDIR/modules" ]; then
	find "$KDIR/modules" -type f -exec cp {} "$ROOT/modules/" \;
fi
# Owned by root and world-readable: the tests run as root without
# CAP_DAC_OVERRIDE, so a root directory owned by the builder would lock
# them out of everything.
chmod -R a+rX "$ROOT"
(cd "$ROOT" && find . | cpio -o -H newc -R 0:0 --quiet | gzip -1 >"$ROOT.initrd")

ACCEL=(-accel tcg)
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
	ACCEL=(-enable-kvm -cpu host)
else
	echo "run.sh: no access to /dev/kvm; emulating (slow)" >&2
fi
timeout "${BPF_VM_TIMEOUT:-900}" qemu-system-x86_64 "${ACCEL[@]}" -m 2G -smp 2 \
	-nographic -no-reboot -kernel "$KDIR/vmlinuz" -initrd "$ROOT.initrd" \
	-append "console=ttyS0 quiet panic=-1" >"$OUT" 2>&1 || true
tr -d '\r' <"$OUT" | grep -v $'\e'
if ! grep -q '^VMTEST-DONE' "$OUT"; then
	echo "run.sh: the VM did not finish"
	exit 2
fi
! grep -qE '^VMTEST [1-9]' "$OUT"
