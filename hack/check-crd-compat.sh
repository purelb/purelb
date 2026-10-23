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
# Fail if PureLB's CRDs changed incompatibly since a baseline commit: a
# field, version or CRD removed, a field newly required, an enum narrowed,
# a limit tightened (hack/crd-compat.py lists the rules). Both sides are
# generated from the Go types with the same controller-gen, so only the
# types' changes show.
#
# Usage: CONTROLLER_GEN="<controller-gen command>" hack/check-crd-compat.sh <baseline-ref>
set -euo pipefail

base=${1:?baseline git ref}
CONTROLLER_GEN=${CONTROLLER_GEN:-controller-gen}
root=$(git rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$work/src" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

git -C "$root" worktree add --detach "$work/src" "$base" >/dev/null 2>&1
echo "baseline: $base ($(git -C "$root" rev-parse --short "$base"))"
(cd "$work/src" && $CONTROLLER_GEN crd paths="./pkg/apis/..." output:crd:artifacts:config="$work/old")
(cd "$root" && $CONTROLLER_GEN crd paths="./pkg/apis/..." output:crd:artifacts:config="$work/new")
python3 "$root/hack/crd-compat.py" "$work/old" "$work/new"
