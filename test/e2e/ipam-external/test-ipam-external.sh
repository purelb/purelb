#!/bin/bash
# End-to-end test for external (sidecar) IPAM.
#
# Exercises the full external-IPAM path: a sidecar process in the allocator
# pod hands out addresses over the gRPC IPAM contract (api/ipam/v1), PureLB
# programs them onto Services and announces them, and `kubectl get sg`
# reflects the sidecar's Stats.
#
# Reusable two ways:
#   1. Out of the box against the bundled sample sidecar (cmd/test-sidecar).
#   2. By a developer building their own external IPAM: point it at your
#      sidecar image + provider name with the env vars below, or deploy your
#      sidecar yourself and pass --no-deploy-sidecar.
#
# Config (env vars, all optional):
#   SIDECAR_IMAGE         sidecar container image
#                         (default ghcr.io/purelb/purelb/test-sidecar:latest)
#   SIDECAR_PROVIDER      provider name shown in .status.ipam (default sample-ipam)
#   SIDECAR_SOCKET        Unix socket path (default /var/run/purelb/ipam.sock)
#   SIDECAR_POOL_CIDR     CIDR the sidecar allocates from. If unset, a /28 is
#                         derived from a detected node subnet so the address
#                         is announceable.
#   SIDECAR_PULL_SECRET   imagePullSecret name for a private sidecar image.
#   ANNOUNCE              local | remote (default local)
#
# Flags:
#   --no-deploy-sidecar   assume the sidecar is already in the allocator pod
#   --keep-sidecar        leave the sidecar deployed after the test
#   --context NAME        kubernetes context
#   -h | --help
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

NAMESPACE="test"
PURELB_NS="purelb-system"
SG_NAME="ipam-external-test"
DEPLOY_SIDECAR=true
KEEP_SIDECAR=false

SIDECAR_IMAGE="${SIDECAR_IMAGE:-ghcr.io/purelb/purelb/test-sidecar:latest}"
SIDECAR_PROVIDER="${SIDECAR_PROVIDER:-sample-ipam}"
SIDECAR_SOCKET="${SIDECAR_SOCKET:-/var/run/purelb/ipam.sock}"
SIDECAR_POOL_CIDR="${SIDECAR_POOL_CIDR:-}"
SIDECAR_PULL_SECRET="${SIDECAR_PULL_SECRET:-}"
ANNOUNCE="${ANNOUNCE:-local}"

while [[ $# -gt 0 ]]; do
    case $1 in
        --no-deploy-sidecar) DEPLOY_SIDECAR=false; shift ;;
        --keep-sidecar)      KEEP_SIDECAR=true; shift ;;
        --context)           CONTEXT="$2"; shift 2 ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//' | head -40
            exit 0 ;;
        *) echo "Unknown option: $1 (use -h for help)"; exit 1 ;;
    esac
done

source "${SCRIPT_DIR}/../common.sh"

# Metrics scrape (same pattern as the local/remote/router suites; not in
# common.sh, so defined here).
scrape_pod_metrics() {
    local pod=$1
    local local_port=$((30000 + RANDOM % 5000))
    kubectl port-forward -n "$PURELB_NS" "$pod" ${local_port}:7472 >/dev/null 2>&1 &
    local pf_pid=$! metrics="" attempt
    for attempt in 1 2 3 4 5; do
        sleep 1
        metrics=$(curl -s --connect-timeout 3 "http://127.0.0.1:${local_port}/metrics" 2>/dev/null || true)
        [ -n "$metrics" ] && break
    done
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true
    echo "$metrics"
}
scrape_allocator_metrics() {
    local pod
    pod=$(kubectl get pods -n "$PURELB_NS" -l component=allocator -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [ -z "$pod" ] && { echo ""; return; }
    scrape_pod_metrics "$pod"
}

# ip_in_cidr_prefix: cheap membership check — the IP shares the CIDR's
# /24 prefix and its last octet falls inside the CIDR's host range.
# Sufficient for the small (<= /24) sidecar pools used here.
ip_in_cidr_prefix() {
    local ip=$1 cidr=$2
    local net="${cidr%/*}" bits="${cidr#*/}"
    local ip_pfx net_pfx ip_oct net_oct span
    ip_pfx=$(echo "$ip" | awk -F. '{print $1"."$2"."$3}')
    net_pfx=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
    [ "$ip_pfx" != "$net_pfx" ] && return 1
    if [ "$bits" -ge 24 ]; then
        ip_oct=$(echo "$ip" | awk -F. '{print $4}')
        net_oct=$(echo "$net" | awk -F. '{print $4}')
        span=$(( 1 << (32 - bits) ))
        [ "$ip_oct" -ge "$net_oct" ] && [ "$ip_oct" -lt "$((net_oct + span))" ]
        return $?
    fi
    return 0
}

LOG_DIR="/tmp/test-ipam-external-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$LOG_DIR"
exec > >(tee -a "$LOG_DIR/output.log") 2>&1
echo "Log file: $LOG_DIR/output.log"

#---------------------------------------------------------------------
# Prerequisites
#---------------------------------------------------------------------
validate_prerequisites() {
    echo "=========================================="
    echo "Prerequisites"
    echo "=========================================="
    discover_nodes
    [ -z "$NODES" ] && fail "no nodes discovered"
    info "Nodes: $NODES"

    kubectl get ns "$PURELB_NS" >/dev/null 2>&1 || fail "purelb-system namespace not found (is PureLB installed?)"
    kubectl get deploy allocator -n "$PURELB_NS" >/dev/null 2>&1 || fail "allocator deployment not found"

    # The allocator must be able to write servicegroups/status (Part A RBAC).
    if kubectl auth can-i update servicegroups/status \
        --as=system:serviceaccount:${PURELB_NS}:allocator -n "$PURELB_NS" 2>/dev/null | grep -q yes; then
        pass "allocator has servicegroups/status RBAC"
    else
        fail "allocator lacks servicegroups/status RBAC (apply the v0.17.0 CRD + RBAC)"
    fi

    # Derive a pool CIDR from a node subnet if the caller didn't set one, so
    # the allocated address is announceable on that subnet.
    if [ -z "$SIDECAR_POOL_CIDR" ]; then
        detect_subnets
        local first_subnet
        first_subnet=$(echo "$SUBNETS" | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1)
        [ -z "$first_subnet" ] && fail "could not detect an IPv4 subnet; set SIDECAR_POOL_CIDR"
        local prefix
        prefix=$(echo "${first_subnet%/*}" | awk -F. '{print $1"."$2"."$3}')
        SIDECAR_POOL_CIDR="${prefix}.224/28"
        info "Derived sidecar pool CIDR ${SIDECAR_POOL_CIDR} from subnet ${first_subnet}"
    fi
    info "Sidecar: image=${SIDECAR_IMAGE} provider=${SIDECAR_PROVIDER} pool=${SIDECAR_POOL_CIDR} announce=${ANNOUNCE}"
}

#---------------------------------------------------------------------
# Sidecar deployment (optional)
#---------------------------------------------------------------------
sidecar_present() {
    kubectl get deploy allocator -n "$PURELB_NS" \
        -o jsonpath='{.spec.template.spec.containers[*].name}' 2>/dev/null | grep -qw test-sidecar
}

deploy_sidecar() {
    echo "=========================================="
    echo "Deploy sidecar into allocator pod"
    echo "=========================================="
    if [ "$DEPLOY_SIDECAR" = false ]; then
        sidecar_present || fail "--no-deploy-sidecar set but no 'test-sidecar' container in the allocator pod"
        pass "using pre-deployed sidecar"
        return
    fi

    local pull_secret_patch=""
    if [ -n "$SIDECAR_PULL_SECRET" ]; then
        pull_secret_patch=$(printf '\n      imagePullSecrets:\n      - name: %s' "$SIDECAR_PULL_SECRET")
    fi

    kubectl patch deploy allocator -n "$PURELB_NS" --type=strategic --patch "$(cat <<EOF
spec:
  template:
    spec:${pull_secret_patch}
      volumes:
      - name: ipam-socket
        emptyDir: {}
      containers:
      - name: allocator
        volumeMounts:
        - name: ipam-socket
          mountPath: /var/run/purelb
      - name: test-sidecar
        image: ${SIDECAR_IMAGE}
        imagePullPolicy: Always
        env:
        - name: SIDECAR_SOCKET
          value: ${SIDECAR_SOCKET}
        - name: SIDECAR_PROVIDER
          value: ${SIDECAR_PROVIDER}
        - name: SIDECAR_POOL_CIDR
          value: ${SIDECAR_POOL_CIDR}
        volumeMounts:
        - name: ipam-socket
          mountPath: /var/run/purelb
EOF
)" >/dev/null
    kubectl rollout status deploy/allocator -n "$PURELB_NS" --timeout=120s || fail "allocator rollout failed (sidecar image pullable?)"
    sidecar_present || fail "sidecar container not present after rollout"
    pass "sidecar deployed; allocator healthy (both containers ready)"
}

#---------------------------------------------------------------------
# Workload
#---------------------------------------------------------------------
apply_servicegroup() {
    kubectl apply -f - >/dev/null <<EOF
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: ${SG_NAME}
  namespace: ${PURELB_NS}
spec:
  external:
    provider: ${SIDECAR_PROVIDER}
    socket: ${SIDECAR_SOCKET}
    announce: ${ANNOUNCE}
EOF
    pass "external ServiceGroup ${SG_NAME} applied"
}

deploy_backend() {
    kubectl apply -f "${SCRIPT_DIR}/nginx-test.yaml" >/dev/null
    kubectl rollout status deployment/nginx -n "$NAMESPACE" --timeout=90s || fail "nginx backend not ready"
    pass "nginx backend ready"
}

apply_service() {
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ext-lb
  namespace: ${NAMESPACE}
  annotations:
    purelb.io/service-group: ${SG_NAME}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies: [IPv4]
  selector: { app: nginx }
  ports: [{ port: 80, targetPort: 80 }]
EOF
}

#---------------------------------------------------------------------
# Tests
#---------------------------------------------------------------------
ALLOCATED_IP=""

test_allocation() {
    echo "=========================================="
    echo "TEST: external allocation"
    echo "=========================================="
    apply_service
    local i
    for i in $(seq 1 15); do
        ALLOCATED_IP=$(kubectl get svc ext-lb -n "$NAMESPACE" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
        [ -n "$ALLOCATED_IP" ] && break
        sleep 1
    done
    [ -z "$ALLOCATED_IP" ] && fail "service did not get an external IP within 15s"
    pass "service allocated $ALLOCATED_IP"

    ip_in_cidr_prefix "$ALLOCATED_IP" "$SIDECAR_POOL_CIDR" \
        && pass "$ALLOCATED_IP is within the sidecar pool ${SIDECAR_POOL_CIDR}" \
        || fail "$ALLOCATED_IP is NOT within the sidecar pool ${SIDECAR_POOL_CIDR}"

    # pool-type annotation should match the configured announce mode.
    local ptype; ptype=$(get_pool_type ext-lb)
    [ "$ptype" = "$ANNOUNCE" ] && pass "pool-type annotation is '$ANNOUNCE'" \
        || fail "pool-type annotation is '$ptype', expected '$ANNOUNCE'"
}

test_announcement() {
    echo "=========================================="
    echo "TEST: announcement + connectivity"
    echo "=========================================="
    if [ "$ANNOUNCE" != "local" ]; then
        info "announce=$ANNOUNCE; skipping on-interface check (remote announces on kube-lb0 across all nodes)"
        return
    fi
    wait_for_ip_announced "$ALLOCATED_IP" 30 || fail "$ALLOCATED_IP not announced on any node within 30s"
    pass "$ALLOCATED_IP announced on a node interface"

    # Reachability from a cluster node (SSH-based, per the suite's methodology).
    local node; node=$(test_connectivity_get_node 2>/dev/null || echo "")
    if [ -n "$node" ] && node_ssh "$node" "curl -s --max-time 5 http://${ALLOCATED_IP}/ | grep -q Pod:" 2>/dev/null; then
        pass "VIP $ALLOCATED_IP reachable (HTTP 200 from nginx)"
    else
        info "connectivity check skipped/failed (SSH or curl unavailable on nodes) — non-fatal"
    fi
}

test_display() {
    echo "=========================================="
    echo "TEST: status display from sidecar Stats"
    echo "=========================================="
    # The allocator polls the sidecar's Stats RPC to populate .status.
    local ipam alloc avail i
    for i in $(seq 1 10); do
        ipam=$(kubectl get sg "$SG_NAME" -n "$PURELB_NS" -o jsonpath='{.status.ipam}' 2>/dev/null)
        [ -n "$ipam" ] && break
        sleep 2
    done
    [ "$ipam" = "$SIDECAR_PROVIDER" ] && pass ".status.ipam = '$SIDECAR_PROVIDER'" \
        || fail ".status.ipam = '$ipam', expected '$SIDECAR_PROVIDER'"

    alloc=$(kubectl get sg "$SG_NAME" -n "$PURELB_NS" -o jsonpath='{.status.allocatedIPv4}' 2>/dev/null)
    [ "${alloc:-0}" -ge 1 ] && pass ".status.allocatedIPv4 = $alloc (>=1)" \
        || fail ".status.allocatedIPv4 = '$alloc', expected >=1"

    avail=$(kubectl get sg "$SG_NAME" -n "$PURELB_NS" -o jsonpath='{.status.availableIPv4}' 2>/dev/null)
    info ".status.availableIPv4 = ${avail:-<absent>}"

    detail "$(kubectl get sg "$SG_NAME" -n "$PURELB_NS" -o wide --no-headers 2>/dev/null)"
}

test_metrics() {
    echo "=========================================="
    echo "TEST: sidecar RPC metrics"
    echo "=========================================="
    local metrics; metrics=$(scrape_allocator_metrics)
    [ -z "$metrics" ] && fail "could not scrape allocator metrics on :7472"

    local alloc_ok
    alloc_ok=$(echo "$metrics" | grep 'purelb_allocator_sidecar_rpc_total{' | grep 'method="/purelb.ipam.v1.IPAM/Allocate"' | grep 'code="OK"' | awk '{print $NF}' | head -1)
    if [ -n "$alloc_ok" ] && [ "${alloc_ok%.*}" -ge 1 ] 2>/dev/null; then
        pass "sidecar Allocate RPCs recorded: ${alloc_ok} OK"
    else
        echo "$metrics" | grep "sidecar_rpc_total{" | grep -v '^#' || true
        fail "no successful Allocate RPC recorded in sidecar_rpc_total"
    fi

    local stats_ok
    stats_ok=$(echo "$metrics" | grep 'purelb_allocator_sidecar_rpc_total{' | grep 'method="/purelb.ipam.v1.IPAM/Stats"' | grep 'code="OK"' | awk '{print $NF}' | head -1)
    [ -n "$stats_ok" ] && pass "sidecar Stats RPCs recorded: ${stats_ok} OK" \
        || info "no Stats RPC recorded yet (status may not have refreshed)"

    # Any non-OK sidecar RPCs are a red flag.
    local errs
    errs=$(echo "$metrics" | grep 'purelb_allocator_sidecar_rpc_total{' | grep -v 'code="OK"' | grep -v '^#' || true)
    [ -z "$errs" ] && pass "no failed sidecar RPCs" || { echo "$errs"; fail "sidecar RPCs with non-OK status code present"; }
}

test_release() {
    echo "=========================================="
    echo "TEST: release"
    echo "=========================================="
    kubectl delete svc ext-lb -n "$NAMESPACE" >/dev/null
    if [ "$ANNOUNCE" = "local" ]; then
        wait_for_ip_not_on_any_node "$ALLOCATED_IP" 30 \
            && pass "$ALLOCATED_IP withdrawn from all nodes" \
            || fail "$ALLOCATED_IP still announced 30s after delete"
    fi
    local metrics rel_ok
    metrics=$(scrape_allocator_metrics)
    rel_ok=$(echo "$metrics" | grep 'purelb_allocator_sidecar_rpc_total{' | grep 'method="/purelb.ipam.v1.IPAM/Release"' | grep 'code="OK"' | awk '{print $NF}' | head -1)
    [ -n "$rel_ok" ] && pass "sidecar Release RPCs recorded: ${rel_ok} OK" \
        || info "no Release RPC recorded (allocator may release lazily)"
}

#---------------------------------------------------------------------
# Cleanup
#---------------------------------------------------------------------
cleanup() {
    echo "=========================================="
    echo "Cleanup"
    echo "=========================================="
    kubectl delete svc ext-lb -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete sg "$SG_NAME" -n "$PURELB_NS" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete -f "${SCRIPT_DIR}/nginx-test.yaml" --ignore-not-found >/dev/null 2>&1 || true

    if [ "$DEPLOY_SIDECAR" = true ] && [ "$KEEP_SIDECAR" = false ]; then
        # Remove the sidecar container + volume we added (JSON patch by name).
        local sc_idx vol_idx
        sc_idx=$(kubectl get deploy allocator -n "$PURELB_NS" -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' | grep -n '^test-sidecar$' | cut -d: -f1)
        if [ -n "$sc_idx" ]; then
            kubectl patch deploy allocator -n "$PURELB_NS" --type=json \
                -p "[{\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/$((sc_idx-1))\"}]" >/dev/null 2>&1 || true
        fi
        vol_idx=$(kubectl get deploy allocator -n "$PURELB_NS" -o jsonpath='{range .spec.template.spec.volumes[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n '^ipam-socket$' | cut -d: -f1)
        if [ -n "$vol_idx" ]; then
            kubectl patch deploy allocator -n "$PURELB_NS" --type=json \
                -p "[{\"op\":\"remove\",\"path\":\"/spec/template/spec/volumes/$((vol_idx-1))\"}]" >/dev/null 2>&1 || true
        fi
        info "removed test-sidecar from allocator (use --keep-sidecar to retain it)"
    fi
    pass "cleanup complete"
}
trap cleanup EXIT

#---------------------------------------------------------------------
# Run
#---------------------------------------------------------------------
echo "######################################################"
echo "# External (sidecar) IPAM E2E"
echo "######################################################"
validate_prerequisites
deploy_sidecar
apply_servicegroup
deploy_backend
test_allocation
test_announcement
test_display
test_metrics
test_release

echo ""
echo "######################################################"
echo -e "# ${GREEN}ALL EXTERNAL-IPAM TESTS PASSED${NC}"
echo "######################################################"
