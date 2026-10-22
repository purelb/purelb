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
# Fail if a core package depends on egress. The allocator, the announcers,
# the address guard and internal/k8s reach egress only through
# internal/egresshook, so a build without egress stays mechanical. Only
# internal/egress itself and the binaries in cmd/ may import it.
#
# Usage: hack/check-egress-imports.sh
set -euo pipefail

EGRESS=purelb.io/internal/egress

# Every package outside cmd/, with its transitive dependencies.
bad=$(go list -f '{{.ImportPath}}{{range .Deps}} {{.}}{{end}}' ./api/... ./internal/... ./pkg/... |
	awk -v e="$EGRESS" '
		$1 == e || index($1, e "/") == 1 { next }   # egress itself
		{
			for (i = 2; i <= NF; i++)
				if ($i == e || index($i, e "/") == 1) { print $1 " -> " $i; next }
		}')

if [ -n "$bad" ]; then
	echo "These packages depend on egress; use internal/egresshook instead:"
	echo "$bad"
	exit 1
fi
echo "ok   no core package depends on $EGRESS"
