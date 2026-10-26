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
# Build every kustomize install variant and check what it contains. The
# release builds them only on a tag, so a broken variant would otherwise
# surface at release time.
#
#   everything (install-, install-crds-):          PureLB + k8gobgp + egress
#   noegress   (install-noegress-, install-crds-noegress-): PureLB + k8gobgp
#   nobgp      (install-nobgp-, install-crds-nobgp-):        PureLB only
#
# Usage: KUSTOMIZE="<kustomize command>" hack/check-manifest-variants.sh
# (after `make crd`)
set -euo pipefail

KUSTOMIZE=${KUSTOMIZE:-kustomize}
fail=0

EGRESS_CRDS="clusteregresses.purelb.io egressaddresses.purelb.io egressendpoints.purelb.io egresses.purelb.io egressmembers.purelb.io egressnodestates.purelb.io"
CORE_CRDS="lbnodeagents.purelb.io servicegroups.purelb.io"
BGP_CRDS="bgpnodestatuses.bgp.purelb.io configs.bgp.purelb.io"

# names: kustomize dir -> sorted "Kind/name" lines
names() {
	$KUSTOMIZE build --load-restrictor LoadRestrictionsNone "deployments/$1" | python3 -c '
import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d:
        print(d["kind"] + "/" + d["metadata"]["name"])' | sort
}

# check: variant, its object list, then "+Kind/name" (must be present) or
# "-pattern" (no object may match) assertions
check() {
	local variant=$1 objs=$2; shift 2
	for a in "$@"; do
		case $a in
		+*) grep -qxF -- "${a#+}" <<<"$objs" || { echo "FAIL $variant: missing ${a#+}" >&2; fail=1; } ;;
		-*) if grep -qE -- "${a#-}" <<<"$objs"; then echo "FAIL $variant: has $(grep -E -- "${a#-}" <<<"$objs" | head -1)" >&2; fail=1; fi ;;
		esac
	done
	echo "ok   $variant: $(wc -l <<<"$objs") objects"
}

crd() { for c in "$@"; do printf '+CustomResourceDefinition/%s ' "$c"; done; }

everything=$(names with-gobgp-egress)
check everything "$everything" $(crd $CORE_CRDS $BGP_CRDS $EGRESS_CRDS) \
	+ClusterRole/purelb:egress:allocator +ClusterRole/purelb:egress:lbnodeagent +ClusterRole/purelb:k8gobgp \
	+FlowSchema/purelb-lbnodeagent-leases \
	+ValidatingAdmissionPolicy/purelb-egress-node-label +ValidatingAdmissionPolicyBinding/purelb-egress-node-label

noegress=$(names with-gobgp)
check noegress "$noegress" $(crd $CORE_CRDS $BGP_CRDS) +ClusterRole/purelb:k8gobgp \
	+FlowSchema/purelb-lbnodeagent-leases -egress

nobgp=$(names default)
check nobgp "$nobgp" $(crd $CORE_CRDS) +FlowSchema/purelb-lbnodeagent-leases -egress -gobgp

check crds-everything "$(names crds-all)" $(crd $CORE_CRDS $BGP_CRDS $EGRESS_CRDS)
check crds-noegress "$(names crds-noegress)" $(crd $CORE_CRDS $BGP_CRDS) -egress
check crds-nobgp "$(names crds)" $(crd $CORE_CRDS) -egress -bgp

# Everything is noegress plus the egress component's objects, nothing else.
extra=$(comm -13 <(echo "$noegress") <(echo "$everything"))
unexpected=$(grep -v egress <<<"$extra" || true)
if [ -n "$unexpected" ]; then
	echo "FAIL everything adds objects that aren't egress's:" >&2; echo "$unexpected" >&2; fail=1
fi
missing=$(comm -23 <(echo "$noegress") <(echo "$everything"))
if [ -n "$missing" ]; then
	echo "FAIL everything lacks noegress objects:" >&2; echo "$missing" >&2; fail=1
fi
echo "ok   everything = noegress + $(wc -l <<<"$extra") egress objects"

exit $fail
