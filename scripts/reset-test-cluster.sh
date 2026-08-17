#!/usr/bin/env bash
# Copyright 2020-2026 Acnodal Inc.
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
# Reset a PureLB test cluster to a known baseline before an e2e run.
#
# The e2e suites generate their own `default` ServiceGroup, so a full
# delete of those is correct and is what makes runs reproducible. The
# default LBNodeAgent is deleted too -- a suite may have left a drifted
# one behind -- but it is then RESTORED to the shipped spec, because
# nothing else creates it: the pytest harness's `lbnodeagent` fixture
# applies its own over the top, and the remaining bash suites assume one
# is already there.
#
# Leftover fixtures are not merely untidy: a ServiceGroup whose ranges
# overlap the ones a suite is about to create is rejected outright by the
# allocator, and a stray LoadBalancer Service holding an address from the
# generated `default` pool skews the addresses_in_use deltas the suites
# assert on.
#
# This is deliberately destructive and deliberately explicit: it requires
# --context so it can never act on whatever kubectl happens to point at,
# and it prints everything it will remove before removing any of it.

set -euo pipefail

# Resolved from the script's own location so the manifests are found
# regardless of the caller's working directory.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info()  { echo -e "${YELLOW}→${NC} $1"; }
ok()    { echo -e "${GREEN}✓${NC} $1"; }
die()   { echo -e "${RED}✗${NC} $1" >&2; exit 1; }

CONTEXT=""
PURELB_NS="purelb-system"
ASSUME_YES=0
# Scratch namespaces only. NOT "test": it holds the echo backend the
# suites require but do not create (test/e2e/echo-test.yaml), so
# deleting it makes every suite fail prerequisite validation. Its
# LoadBalancer Services are removed above like any other.
EXTRA_NS=("test-tenant" "echo-test")

usage() {
    cat <<'EOF'
Usage: reset-test-cluster.sh --context NAME [--yes] [--namespace NS]

Removes, so an e2e run starts from a known baseline:
  - every Gateway (its controller would otherwise recreate the Service)
  - every LoadBalancer Service outside the PureLB install namespace
  - every ServiceGroup (the suites generate their own)
  - every LBNodeAgent, then restores the shipped `default` one
  - the e2e scratch namespaces (test-tenant, echo-test)
  - purelb-test NoExecute taints left by a failed failover test

Then verifies no PureLB-assigned address is still configured on any node.

  --context NAME   kubectl context (REQUIRED; no default, by design)
  --yes            do not prompt before deleting
  --namespace NS   PureLB install namespace (default: purelb-system)
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --context)   CONTEXT="${2:-}"; shift 2 ;;
        --namespace) PURELB_NS="${2:-}"; shift 2 ;;
        --yes|-y)    ASSUME_YES=1; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)           usage; die "unknown argument: $1" ;;
    esac
done

# No default context. Defaulting to the current context is how a reset
# script eventually runs against something it should not have.
[ -n "$CONTEXT" ] || { usage; die "--context is required"; }

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

kubectl cluster-info >/dev/null 2>&1 || die "cannot reach cluster for context '$CONTEXT'"

# ---------------------------------------------------------------------
# Survey first, delete second
# ---------------------------------------------------------------------

echo "=== Reset plan for context '$CONTEXT' ==="
echo

# Capture the addresses PureLB has handed out BEFORE deleting anything.
# After the delete there is no way to reconstruct which addresses were
# ours, so the post-condition check has to be primed here.
ASSIGNED_IPS=$(kubectl get svc -A -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{range .status.loadBalancer.ingress[*]}{.ip}{"\n"}{end}{end}' 2>/dev/null | grep -v '^$' || true)

LB_SVCS=$(kubectl get svc -A -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -v "^${PURELB_NS}/" | grep -v '^/$' || true)

# Gateway API resources are the root cause of a LoadBalancer Service that
# reappears seconds after deletion: envoy-gateway (and any other Gateway
# controller) owns the Service and recreates it. Deleting only the Service
# leaves a controller racing the reset -- observed re-grabbing the first
# address of the e2e pool band before the ServiceGroup had finished
# deleting. Delete the Gateway itself so nothing recreates it.
GATEWAYS=""
if kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1; then
    GATEWAYS=$(kubectl get gateways.gateway.networking.k8s.io -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^/$' || true)
fi
SGS=$(kubectl get servicegroup -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^/$' || true)
AGENTS=$(kubectl get lbnodeagent -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^/$' || true)
TAINTED=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .spec.taints[*]}{.key}{" "}{end}{"\n"}{end}' 2>/dev/null | grep 'purelb-test' | awk '{print $1}' || true)

print_block() {
    local title="$1" body="$2"
    if [ -n "$body" ]; then
        echo "  $title:"
        echo "$body" | sed 's/^/    - /'
    else
        echo "  $title: (none)"
    fi
}

print_block "Gateways (recreate their Services if left)" "$GATEWAYS"
print_block "LoadBalancer Services outside ${PURELB_NS}" "$LB_SVCS"
print_block "ServiceGroups" "$SGS"
print_block "LBNodeAgents" "$AGENTS"
print_block "Nodes with purelb-test taints" "$TAINTED"
echo "  Scratch namespaces: ${EXTRA_NS[*]} (deleted if present)"
echo

if [ "$ASSUME_YES" -ne 1 ]; then
    read -r -p "Proceed with deletion? [y/N] " reply
    case "$reply" in [yY]*) ;; *) die "aborted" ;; esac
fi

# ---------------------------------------------------------------------
# Delete
# ---------------------------------------------------------------------

if [ -n "$GATEWAYS" ]; then
    while IFS= read -r gw; do
        ns="${gw%%/*}"; name="${gw##*/}"
        info "deleting Gateway $gw"
        kubectl delete gateways.gateway.networking.k8s.io "$name" -n "$ns" --ignore-not-found --timeout=60s >/dev/null
    done <<< "$GATEWAYS"
fi

if [ -n "$LB_SVCS" ]; then
    while IFS= read -r svc; do
        ns="${svc%%/*}"; name="${svc##*/}"
        info "deleting Service $svc"
        kubectl delete svc "$name" -n "$ns" --ignore-not-found --timeout=60s >/dev/null
    done <<< "$LB_SVCS"
fi

if [ -n "$SGS" ]; then
    while IFS= read -r sg; do
        ns="${sg%%/*}"; name="${sg##*/}"
        info "deleting ServiceGroup $sg"
        kubectl delete servicegroup "$name" -n "$ns" --ignore-not-found --timeout=60s >/dev/null
    done <<< "$SGS"
fi

if [ -n "$AGENTS" ]; then
    while IFS= read -r a; do
        ns="${a%%/*}"; name="${a##*/}"
        info "deleting LBNodeAgent $a"
        kubectl delete lbnodeagent "$name" -n "$ns" --ignore-not-found --timeout=60s >/dev/null
    done <<< "$AGENTS"
fi

# Restore the shipped default LBNodeAgent. Deleting every agent and
# stopping there does not leave a baseline -- it leaves PureLB installed
# and unable to announce anything, because an LBNodeAgent CR is what
# configures the node agents at all. The pytest harness applies its own
# over the top; the remaining bash suites assume one is already there, so
# without this they allocate an address and then fail at "not announced
# on any node" -- which is how this was found.
#
# Spec matches deployments/purelb-v0.0.0-dev.yaml, so the baseline is what
# PureLB actually ships rather than a definition invented here. A suite
# that wants different settings applies its own over the top.
info "restoring the default LBNodeAgent"
kubectl apply -f - >/dev/null <<EOF
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: ${PURELB_NS}
  labels:
    app: purelb
spec:
  local:
    dummyInterface: kube-lb0
    localInterface: default
EOF

# A Gateway delete removes its Service asynchronously; sweep once more so
# the post-condition check below is not racing a controller.
LEFTOVER=$(kubectl get svc -A -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -v "^${PURELB_NS}/" | grep -v '^/$' || true)
if [ -n "$LEFTOVER" ]; then
    while IFS= read -r svc; do
        ns="${svc%%/*}"; name="${svc##*/}"
        info "deleting leftover Service $svc"
        kubectl delete svc "$name" -n "$ns" --ignore-not-found --timeout=60s >/dev/null
    done <<< "$LEFTOVER"
fi

for ns in "${EXTRA_NS[@]}"; do
    if kubectl get namespace "$ns" >/dev/null 2>&1; then
        info "deleting namespace $ns"
        kubectl delete namespace "$ns" --ignore-not-found --timeout=120s >/dev/null
    fi
done

if [ -n "$TAINTED" ]; then
    while IFS= read -r node; do
        info "removing purelb-test taint from $node"
        kubectl taint node "$node" purelb-test- >/dev/null 2>&1 || true
    done <<< "$TAINTED"
fi

kubectl label node --all purelb-test- >/dev/null 2>&1 || true

# ---------------------------------------------------------------------
# Verify the post-condition
#
# Deleting the Kubernetes objects is not the same as the data plane
# having converged. An address left on a NIC after its Service is gone
# is exactly the "stale VIP" class the failover tests hunt for, and
# starting a run with one already present makes those results
# meaningless.
# ---------------------------------------------------------------------

echo
info "verifying no PureLB address remains on any node"

mapfile -t NODE_IPS < <(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' 2>/dev/null | grep -v '^$')

STALE=0
if [ -z "$ASSIGNED_IPS" ]; then
    ok "no LoadBalancer addresses were assigned; nothing to check"
else
    for nip in "${NODE_IPS[@]}"; do
        addrs=$(ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
                    "$nip" 'ip -br addr show' 2>/dev/null || echo "__SSH_FAILED__")
        if [ "$addrs" = "__SSH_FAILED__" ]; then
            echo -e "${YELLOW}→${NC} WARNING: could not ssh to $nip; stale-address check skipped for it"
            continue
        fi
        while IFS= read -r ip; do
            [ -n "$ip" ] || continue
            if echo "$addrs" | tr ' ' '\n' | grep -q "^${ip}/"; then
                echo -e "${RED}✗${NC} stale address $ip still configured on $nip"
                STALE=$((STALE + 1))
            fi
        done <<< "$ASSIGNED_IPS"
    done
    [ "$STALE" -eq 0 ] && ok "no stale addresses on any reachable node"
fi

# ---------------------------------------------------------------------
# Baseline
# ---------------------------------------------------------------------

echo
echo "=== Baseline after reset ==="
echo "  ServiceGroups:  $(kubectl get servicegroup -A --no-headers 2>/dev/null | wc -l)"
echo "  LBNodeAgents:   $(kubectl get lbnodeagent -A --no-headers 2>/dev/null | wc -l)"
echo "  LB Services:    $(kubectl get svc -A --no-headers 2>/dev/null | grep -c LoadBalancer || true)"
echo "  PureLB pods:"
kubectl get pods -n "$PURELB_NS" --no-headers 2>/dev/null | awk '{printf "    %-32s %s %s\n", $1, $2, $3}'

# The suites require the echo backend in the `test` namespace and none of
# them creates it, so a baseline without one is not runnable. Restoring it
# here rather than warning about it is also what makes suite ORDER stop
# mattering: the retired bash ipam-external suite deleted the backend on
# its way out, which silently removed a prerequisite for everything that
# ran after it.
#
# Applied unconditionally, unlike the old nginx restore which ran only when
# no pod was up. The server is a ConfigMap, not an image, so "a pod is
# running" says nothing about WHICH server it is running -- an edit to
# server.py must reach the cluster even when the old pod is perfectly
# healthy. The checksum below is what turns that edit into a rollout.
# Remove the nginx backend this replaced. Clusters that ran the suite
# before the switch still have it, and nothing else would ever clean it
# up -- it would sit there indefinitely, answering on `app: nginx`,
# confusing anyone who went looking for the backend.
if kubectl get deployment nginx -n test >/dev/null 2>&1; then
    info "removing the superseded nginx backend"
    kubectl delete deployment nginx -n test --ignore-not-found --timeout=60s >/dev/null
    kubectl delete configmap nginx-config -n test --ignore-not-found >/dev/null
fi

info "restoring the echo backend in namespace 'test'"
kubectl create configmap echo-server -n test \
    --from-file=server.py="${REPO_ROOT}/test/echo-server/server.py" \
    --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null \
    || die "could not create the echo-server ConfigMap"
kubectl apply -f "${REPO_ROOT}/test/e2e/echo-test.yaml" >/dev/null \
    || die "could not apply test/e2e/echo-test.yaml"

# Stamp sha256(server.py) on the pod template. A ConfigMap has no tag and a
# running interpreter never re-reads the file it started from, so without
# this an edited server would sit in the ConfigMap while every pod kept
# serving the old code -- and the suite would be testing something that is
# not in the tree. Patching an unchanged checksum is a no-op; a changed one
# rolls the Deployment.
SUM=$(sha256sum "${REPO_ROOT}/test/echo-server/server.py" | cut -c1-16)
kubectl -n test patch deployment echo --type=merge \
    -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"purelb.io/config-checksum\":\"${SUM}\"}}}}}" \
    >/dev/null || die "could not stamp the echo-server checksum"

kubectl rollout status deployment/echo -n test --timeout=180s >/dev/null \
    || die "echo backend did not become ready"

# Ask the RUNNING process what it loaded, and refuse to hand over a cluster
# where it disagrees with the tree.
#
# The checksum above is the template's opinion; this is the process's. They can
# disagree in the ordinary case where server.py was edited and the pods were
# never rolled, and nothing else in the setup would say so. A reset that cannot
# certify which code is answering is not a baseline, so this is a die, not a
# warning. Uses the pod's own python -- the image has no curl.
RUNNING=$(kubectl exec deploy/echo -n test -- python3 -c \
    "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8080/version').read().decode().strip())" \
    2>/dev/null) || die "could not read /version from the echo backend"
[ "$RUNNING" = "$SUM" ] || die "echo backend is running server.py $RUNNING but the tree has $SUM; the pod did not pick up the new code"

BACKEND=$(kubectl get pods -n test -l app=echo --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
echo "  Backend pods:   $BACKEND running in namespace 'test' (server.py $SUM, verified live)"

if [ "$STALE" -ne 0 ]; then
    die "$STALE stale address(es) remain; the cluster is NOT a clean baseline"
fi
echo
ok "cluster reset to baseline"
