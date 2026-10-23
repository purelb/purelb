#!/usr/bin/env bash
# Measure the egress allocator's API load against a fresh kwok cluster.
#
#   ./run.sh                       # N=500: egress-free baseline, then egress
#   NODES=20 POLICIES=10 PODS=200 RUNS="egress" ./run.sh   # a smoke run
#
# Expects, beside this script: allocator and egress-kwok (linux/amd64
# builds of ./cmd/allocator and ./hack/egress-kwok), crds/ (the repo's
# deployments/crds) and kwokctl (KWOKCTL, default .bin/kwokctl). All kwok
# state lives under ./.work; each run gets a new cluster, deleted after.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
export KWOK_WORKDIR="$DIR/.work"
export KWOK_KUBE_VERSION="${KWOK_KUBE_VERSION:-v1.35.9}"
KWOKCTL="${KWOKCTL:-$DIR/.bin/kwokctl}"
NAME=egress
KCFG="$KWOK_WORKDIR/clusters/$NAME/kubeconfig.yaml"
ETCD_PORT=32379
NODES="${NODES:-500}"
POLICIES="${POLICIES:-250}"
PODS="${PODS:-5000}"
RUNS="${RUNS:-baseline egress}"
EXTRA="${EXTRA:-}"
TAG="${TAG:-$(date +%Y%m%d-%H%M)}"
mkdir -p "$DIR/results"

kwokctl() { "$KWOKCTL" --name "$NAME" "$@"; }
trap 'kwokctl delete cluster >/dev/null 2>&1 || true' EXIT

for R in $RUNS; do
  egress=true
  [[ "$R" == "baseline" ]] && egress=false
  out="$DIR/results/$R-n$NODES-p$POLICIES-$TAG"
  echo "=== $R n=$NODES policies=$POLICIES pods=$PODS ==="
  kwokctl delete cluster >/dev/null 2>&1 || true
  kwokctl create cluster --runtime docker --etcd-port "$ETCD_PORT" --wait 3m
  kwokctl kubectl apply --server-side -f "$DIR/crds/"
  kwokctl kubectl wait --for=condition=Established crd --all --timeout=60s
  "$DIR/egress-kwok" -mode setup -kubeconfig "$KCFG" -egress="$egress" \
    -nodes "$NODES" -policies "$POLICIES" -pods "$PODS"
  (cd "$DIR/results" && "$DIR/egress-kwok" -mode run -kubeconfig "$KCFG" -allocator "$DIR/allocator" \
    -etcd-metrics "http://127.0.0.1:$ETCD_PORT/metrics" -egress="$egress" \
    -nodes "$NODES" -policies "$POLICIES" -pods "$PODS" $EXTRA -out "$out.json") 2>&1 | tee "$out.log"
done
