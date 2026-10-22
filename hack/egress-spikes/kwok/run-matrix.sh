#!/usr/bin/env bash
# Phase 0b spike 8: run the egress control-plane load matrix against kwok.
#
#   ./run-matrix.sh                 # full matrix (N=100,500)
#   NODES="100" ./run-matrix.sh     # subset
#
# Everything (kwok state, kubeconfig, PKI) lives under ./.work; nothing is
# written to ~/.kube or ~/.kwok. The cluster is deleted at the end.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
export KWOK_WORKDIR="$DIR/.work"
export KWOK_KUBE_VERSION="${KWOK_KUBE_VERSION:-v1.35.9}"
KWOK_VERSION="${KWOK_VERSION:-v0.8.0}"
NAME="${NAME:-spike}"
KWOKCTL="$DIR/.bin/kwokctl"
KCFG="$KWOK_WORKDIR/clusters/$NAME/kubeconfig.yaml"
ETCD_PORT=32379
NODES="${NODES:-100 500}"
# "policies:addrsPerPolicy" -> P*A egress addresses (A=2: one IPv4 + one IPv6)
SHAPES="${SHAPES:-50:2 250:2}"
# "qps:burst" for agents and allocator
QPSES="${QPSES:-5:10 50:100}"
DESIGNS="${DESIGNS:-decided orig}"
EXTRA="${EXTRA:-}"
SUFFIX="${SUFFIX:-}"   # appended to result file names (ablations)
KEEP="${KEEP:-0}"

if [[ ! -x "$KWOKCTL" ]]; then
  GOBIN="$DIR/.bin" go install "sigs.k8s.io/kwok/cmd/kwokctl@$KWOK_VERSION"
  GOBIN="$DIR/.bin" go install "sigs.k8s.io/kwok/cmd/kwok@$KWOK_VERSION"
fi
if [[ "${NO_BUILD:-0}" != "1" ]]; then   # NO_BUILD=1: use a prebuilt ./loadgen (a host without Go)
  (cd "$DIR" && go build -o loadgen .)
fi

kwokctl() { "$KWOKCTL" --name "$NAME" "$@"; }

cleanup_cluster() {
  if [[ "$KEEP" != "1" ]]; then
    kwokctl delete cluster || true
  fi
}

# A new cluster for every run: the apiserver keeps its Go heap and etcd its
# cached pages after a run, so a shared cluster makes each run's memory (and
# etcd size) depend on the runs before it.
fresh_cluster() {
  kwokctl delete cluster >/dev/null 2>&1 || true
  kwokctl create cluster --runtime docker \
    --kubeconfig "$KWOK_WORKDIR/kubeconfig" \
    --etcd-port "$ETCD_PORT" --wait 3m
  kwokctl kubectl apply -f "$DIR/crds/crds.yaml"
  kwokctl kubectl wait --for=condition=Established crd --all --timeout=60s
  "$DIR/loadgen" -mode setup -kubeconfig "$KCFG" -nodes "$1"
}

trap cleanup_cluster EXIT
for N in $NODES; do
  for shape in $SHAPES; do
    P=${shape%%:*}; A=${shape##*:}
    for q in $QPSES; do
      QPS=${q%%:*}; BURST=${q##*:}
      for D in $DESIGNS; do
        tag="${D}-n${N}-a$((P*A))-q${QPS}${SUFFIX}"
        if [[ -f "$DIR/results/$tag.json" ]]; then echo "=== $tag exists, skipping ==="; continue; fi
        echo "=== $tag ==="
        fresh_cluster "$N"
        "$DIR/loadgen" -mode run -kubeconfig "$KCFG" -etcd-metrics "http://127.0.0.1:$ETCD_PORT/metrics" \
          -design "$D" -nodes "$N" -policies "$P" -addrs "$A" -pods 5000 \
          -qps "$QPS" -burst "$BURST" $EXTRA \
          -out "$DIR/results/$tag.json" 2>&1 | tee "$DIR/results/$tag.log"
      done
    done
  done
done
