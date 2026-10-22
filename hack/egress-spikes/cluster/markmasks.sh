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
# Resolution 22: which bytes of skb->mark / ct mark does this cluster's
# netfilter configuration use? Read-only.
#
#   hack/egress-spikes/cluster/markmasks.sh "<kubectl args>"
#
# Reads every node's `nft list ruleset` (iptables-nft rules included) and,
# where present, iptables-legacy, collects every constant in a packet-mark or
# conntrack-mark expression, and prints the bits it matches and modifies, which
# of the four bytes carry any bit, and any rule that overwrites or matches the
# whole mark (those clobber or mis-match every byte). Marks a CNI sets from BPF programs do not
# appear here.
set -euo pipefail
K="kubectl $1"
mapfile -t NODES < <($K get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | awk '{print $1, $2}')
for l in "${NODES[@]}"; do
	set -- $l
	ssh -n -o BatchMode=yes "$2" 'sudo -n nft list ruleset 2>/dev/null; command -v iptables-legacy-save >/dev/null && { sudo -n iptables-legacy-save 2>/dev/null; sudo -n ip6tables-legacy-save 2>/dev/null; }; true' |
		python3 -I -c '
import re, sys
node = sys.argv[1]
F = 0xffffffff
matched = modified = 0
overwrite, fullmatch = set(), set()
H = r"(0x[0-9a-f]+|\d+)"
num = lambda t: int(t, 16) if t.startswith("0x") else int(t)
for raw in sys.stdin:
    line = raw.lower()
    for kind in ("meta mark", "ct mark"):
        for m in re.finditer(kind + r" & " + H + r" (?:==|!=) " + H, line):
            matched |= num(m.group(1))
        for m in re.finditer(kind + r" set (?:meta |ct )?mark (?:and|&) " + H + r"(?: (?:xor|\^|or|\|) " + H + r")?", line):
            modified |= (~num(m.group(1)) & F) | (num(m.group(2)) if m.group(2) else 0)
        for m in re.finditer(kind + r" set (?:meta |ct )?mark (?:or|\||xor|\^) " + H, line):
            modified |= num(m.group(1))
        for m in re.finditer(kind + r" set " + H + r"(?![\w&|^])", line):
            overwrite.add("%s set 0x%x" % (kind, num(m.group(1))))
        for m in re.finditer(kind + r" (?!set|&|and|or|xor)" + H + r"(?![\w/])", line):
            fullmatch.add("%s 0x%x" % (kind, num(m.group(1))))
    # iptables-legacy syntax
    for m in re.finditer(r"--mark (0x[0-9a-f]+)/(0x[0-9a-f]+)", line):
        matched |= num(m.group(2))
    for m in re.finditer(r"--set-xmark (0x[0-9a-f]+)/(0x[0-9a-f]+)", line):
        modified |= num(m.group(2))
used = matched | modified
by = lambda v: " ".join("0x%08x" % (0xff << (8 * i)) for i in range(4) if v & (0xff << (8 * i))) or "none"
free = [i for i in range(4) if not used & (0xff << (8 * i))]
print(f"{node}: matched 0x{matched:08x} modified 0x{modified:08x}; bytes used: {by(used)}; free: " + (" ".join("0x%08x" % (0xff << (8 * i)) for i in free) or "none"))
if overwrite: print(f"  {node}: whole-mark overwrites: " + ", ".join(sorted(overwrite)))
if fullmatch: print(f"  {node}: whole-mark exact matches: " + ", ".join(sorted(fullmatch)))
' "$1"
done
