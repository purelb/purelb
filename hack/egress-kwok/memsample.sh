#!/usr/bin/env bash
# Sample kube-apiserver memory beside a run.sh run: the container's anon and
# file memory, Go heap, stored objects and watch-cache events per resource
# every 5 s, and a heap profile every 60 s. Ends when the cluster is deleted.
#
#   ./memsample.sh results/mem-egress &   # then ./run.sh with RUNS=egress
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="$1"; mkdir -p "$OUT"
export KWOK_WORKDIR="$DIR/.work"
KCFG="$DIR/.work/clusters/egress/kubeconfig.yaml"
K() { "${KWOKCTL:-$DIR/.bin/kwokctl}" --name egress kubectl "$@"; }
until docker inspect kwok-egress-kube-apiserver >/dev/null 2>&1 && [ -f "$KCFG" ] && K get --raw /readyz >/dev/null 2>&1; do sleep 1; done
CG="/sys/fs/cgroup/system.slice/docker-$(docker inspect -f '{{.Id}}' kwok-egress-kube-apiserver).scope"
start=$(date +%s); n=0
echo "start $(date -Is)" > "$OUT/samples.txt"
while docker inspect kwok-egress-kube-apiserver >/dev/null 2>&1; do
  t=$(( $(date +%s) - start ))
  {
    echo "=== t=$t $(date +%T)"
    grep -E '^(anon|file) ' "$CG/memory.stat"
    echo "memory.current $(cat "$CG/memory.current")"
    K get --raw /metrics 2>/dev/null | grep -E '^(go_memstats_(heap_inuse|heap_alloc|heap_idle|heap_released)_bytes|process_resident_memory_bytes|go_goroutines|apiserver_storage_objects|apiserver_watch_cache_events_received_total)'
  } >> "$OUT/samples.txt"
  if (( n % 12 == 0 )); then
    K get --raw '/debug/pprof/heap' > "$OUT/heap-t$t.pprof" 2>/dev/null
  fi
  n=$((n+1)); sleep 5
done
echo "end $(date -Is)" >> "$OUT/samples.txt"
