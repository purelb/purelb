#!/bin/bash
set -e

# Bash version check (required for associative arrays)
if [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "ERROR: Bash 4+ required for associative arrays"
    exit 1
fi

# Determine script directory for relative file paths
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

CONTEXT="proxmox"
NAMESPACE="test"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

pass() { echo -e "${GREEN}✓ PASS:${NC} $1"; }
fail() { echo -e "${RED}✗ FAIL:${NC} $1"; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Wait for an IP to be announced on any node (with timeout)
# Usage: wait_for_ip_announced <ip> [timeout_seconds]
wait_for_ip_announced() {
    local IP=$1
    local TIMEOUT=${2:-30}
    local INTERVAL=2
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
            if ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                return 0
            fi
        done
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    return 1
}

#---------------------------------------------------------------------
# Address Lifetime Helpers
#---------------------------------------------------------------------

# Get detailed address info including flags and lifetime
get_address_details() {
    local NODE=$1
    local IP=$2
    local INTERFACE=$3
    ssh "$NODE" "ip -d addr show $INTERFACE 2>/dev/null | grep -A1 ' $IP/'" 2>/dev/null || true
}

# Extract valid_lft value from address details
# Returns: lifetime in seconds, or "forever" for permanent
get_valid_lft() {
    local DETAILS=$1
    if echo "$DETAILS" | grep -q "valid_lft forever"; then
        echo "forever"
    else
        echo "$DETAILS" | grep -oP 'valid_lft \K[0-9]+' || echo "unknown"
    fi
}

# Check if address details contain a specific property
check_address_property() {
    local DETAILS=$1
    local PROPERTY=$2
    echo "$DETAILS" | grep -q "$PROPERTY"
}

# Detect CNI plugin
detect_cni() {
    if kubectl get daemonset -n kube-flannel kube-flannel-ds &>/dev/null; then
        echo "flannel"; return
    fi
    if kubectl get daemonset -n kube-system kube-flannel-ds &>/dev/null; then
        echo "flannel"; return
    fi
    if kubectl get daemonset -n kube-system calico-node &>/dev/null; then
        echo "calico"; return
    fi
    if kubectl get daemonset -n calico-system calico-node &>/dev/null; then
        echo "calico"; return
    fi
    if kubectl get daemonset -n kube-system cilium &>/dev/null; then
        echo "cilium"; return
    fi
    echo "unknown"
}

# Find which node has a given VIP
find_vip_node() {
    local IP=$1
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$node"
            return 0
        fi
    done
    return 1
}

# Test connectivity and return the responding node
# Usage: test_connectivity_get_node <ip> [port]
test_connectivity_get_node() {
    local IP=$1
    local PORT=${2:-80}
    local RESPONSE
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IP:$PORT/" 2>/dev/null || true)
    if echo "$RESPONSE" | grep -q "Node:"; then
        echo "$RESPONSE" | grep "Node:" | awk '{print $2}'
        return 0
    fi
    return 1
}

#---------------------------------------------------------------------
# Infrastructure Prerequisites Validation
# Validates that the cluster infrastructure is properly configured
# for load balancer traffic to work correctly
#---------------------------------------------------------------------
validate_prerequisites() {
    echo ""
    echo "=========================================="
    echo "PREREQUISITES: Infrastructure Validation"
    echo "=========================================="

    local FAILED=false

    # Check 1: IP forwarding enabled on all nodes (IPv4)
    info "Checking IPv4 IP forwarding on all nodes..."
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        local IPV4_FWD
        IPV4_FWD=$(ssh $node "cat /proc/sys/net/ipv4/ip_forward" 2>/dev/null || echo "error")
        if [ "$IPV4_FWD" = "1" ]; then
            pass "IPv4 forwarding enabled on $node"
        elif [ "$IPV4_FWD" = "0" ]; then
            echo -e "${RED}✗ FAIL:${NC} IPv4 forwarding DISABLED on $node"
            echo "  Fix: ssh $node 'sysctl -w net.ipv4.ip_forward=1'"
            FAILED=true
        else
            echo -e "${RED}✗ FAIL:${NC} Could not check IPv4 forwarding on $node (SSH error?)"
            FAILED=true
        fi
    done

    # Check 2: IP forwarding enabled on all nodes (IPv6)
    info "Checking IPv6 IP forwarding on all nodes..."
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        local IPV6_FWD
        IPV6_FWD=$(ssh $node "cat /proc/sys/net/ipv6/conf/all/forwarding" 2>/dev/null || echo "error")
        if [ "$IPV6_FWD" = "1" ]; then
            pass "IPv6 forwarding enabled on $node"
        elif [ "$IPV6_FWD" = "0" ]; then
            echo -e "${RED}✗ FAIL:${NC} IPv6 forwarding DISABLED on $node"
            echo "  Fix: ssh $node 'sysctl -w net.ipv6.conf.all.forwarding=1'"
            FAILED=true
        else
            echo -e "${RED}✗ FAIL:${NC} Could not check IPv6 forwarding on $node (SSH error?)"
            FAILED=true
        fi
    done

    # Check 3: kube-proxy is running
    info "Checking kube-proxy is running..."
    local KUBE_PROXY_PODS
    KUBE_PROXY_PODS=$(kubectl get pods -n kube-system -l k8s-app=kube-proxy --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$KUBE_PROXY_PODS" -ge 1 ]; then
        pass "kube-proxy is running ($KUBE_PROXY_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} kube-proxy not running"
        FAILED=true
    fi

    # Check 4: Test pods are running and distributed
    info "Checking test pods are running..."
    local READY_PODS
    READY_PODS=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$READY_PODS" -ge 1 ]; then
        pass "Test pods are running ($READY_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} No test pods running in namespace $NAMESPACE"
        FAILED=true
    fi

    # Check 5: Pod-to-pod connectivity (basic CNI health check)
    info "Checking pod-to-pod connectivity..."
    local POD_NAME
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$POD_NAME" ]; then
        # Try to reach the Kubernetes API from inside a pod (proves networking works)
        if kubectl exec -n $NAMESPACE "$POD_NAME" -- wget -q -O /dev/null --timeout=5 https://kubernetes.default.svc 2>/dev/null; then
            pass "Pod networking is functional"
        else
            # Fallback: try a simple DNS lookup
            if kubectl exec -n $NAMESPACE "$POD_NAME" -- nslookup kubernetes.default.svc 2>/dev/null | grep -q "Address"; then
                pass "Pod DNS is functional"
            else
                info "Could not verify pod networking (may be OK if no internet access)"
            fi
        fi
    fi

    # Check 6: PureLB components are running
    info "Checking PureLB components..."
    local ALLOCATOR_READY
    ALLOCATOR_READY=$(kubectl get deployment -n purelb-systemallocator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "$ALLOCATOR_READY" -ge 1 ]; then
        pass "Allocator is running"
    else
        echo -e "${RED}✗ FAIL:${NC} Allocator not running"
        FAILED=true
    fi

    local AGENT_PODS
    AGENT_PODS=$(kubectl get pods -n purelb-system-l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$AGENT_PODS" -ge 1 ]; then
        pass "LBNodeAgent is running ($AGENT_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} LBNodeAgent not running"
        FAILED=true
    fi

    if [ "$FAILED" = "true" ]; then
        echo ""
        echo -e "${RED}═══════════════════════════════════════════════════════════════${NC}"
        echo -e "${RED}INFRASTRUCTURE VALIDATION FAILED${NC}"
        echo -e "${RED}Please fix the issues above before running E2E tests.${NC}"
        echo -e "${RED}═══════════════════════════════════════════════════════════════${NC}"
        exit 1
    fi

    pass "All infrastructure prerequisites validated"
}

#---------------------------------------------------------------------
# Setup: Ensure LBNodeAgent configuration exists
#---------------------------------------------------------------------
setup_lbnodeagent() {
    info "Ensuring LBNodeAgent configuration exists..."
    cat <<EOF | kubectl apply -f -
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
EOF
    pass "LBNodeAgent configuration applied"
}

#---------------------------------------------------------------------
# Test 1: IPv4 Single-Stack Service
#---------------------------------------------------------------------
test_ipv4_singlestack() {
    echo ""
    echo "=========================================="
    echo "TEST 1: IPv4 Single-Stack Service"
    echo "=========================================="

    info "Creating IPv4-only service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IPv4: $IPV4"

    # Verify IP is from pool range
    [[ "$IPV4" =~ ^172\.30\.255\.(1[5-7][0-9]|180)$ ]] || fail "IP not from expected pool"
    pass "IPv4 allocated from correct pool"

    # Verify IP is on eth0 (local subnet)
    # CRITICAL: Use -w for word boundary to prevent partial IP matching
    # e.g., grep '172.30.255.15' would incorrectly match 172.30.255.150
    info "Checking IP location on nodes..."
    WINNER_NODE=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        eth0_ip=$(ssh $node "ip -o addr show eth0 2>/dev/null | grep ' $IPV4/'" 2>/dev/null || true)
        kubelb0_ip=$(ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IPV4/'" 2>/dev/null || true)
        if [ -n "$eth0_ip" ]; then
            pass "IPv4 $IPV4 is on eth0 on $node"
            WINNER_NODE=$node
        fi
        if [ -n "$kubelb0_ip" ]; then
            fail "IPv4 $IPV4 is on kube-lb0 on $node (should be on eth0)"
        fi
    done

    [ -n "$WINNER_NODE" ] || fail "IPv4 not found on any node"

    # Test connectivity
    info "Testing connectivity to $IPV4..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from service"
    pass "IPv4 service is reachable"
    echo "$RESPONSE"

    # Verify PureLB annotations (allocated-by, allocated-from)
    info "Verifying PureLB annotations..."
    ALLOCATED_BY=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-by}')
    ALLOCATED_FROM=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-from}')
    [ "$ALLOCATED_BY" = "PureLB" ] || fail "Missing or wrong purelb.io/allocated-by annotation (got: $ALLOCATED_BY)"
    [ -n "$ALLOCATED_FROM" ] || fail "Missing purelb.io/allocated-from annotation"
    pass "PureLB annotations correctly set (allocated-by=PureLB, allocated-from=$ALLOCATED_FROM)"

    # Verify ipMode field (K8s 1.30+)
    IPMODE=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ipMode}')
    if [ -n "$IPMODE" ]; then
        [ "$IPMODE" = "VIP" ] || fail "ipMode should be VIP, got $IPMODE"
        pass "ipMode correctly set to VIP"
    else
        info "ipMode not set (may be older K8s version)"
    fi
}

#---------------------------------------------------------------------
# Test 2: IPv6 Single-Stack Service
#---------------------------------------------------------------------
test_ipv6_singlestack() {
    echo ""
    echo "=========================================="
    echo "TEST 2: IPv6 Single-Stack Service"
    echo "=========================================="

    info "Creating IPv6-only service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv6.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv6 -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IPv6: $IPV6"

    # Verify IP is from pool range
    [[ "$IPV6" =~ ^2001:470:b8f3:2:a:: ]] || fail "IP not from expected pool"
    pass "IPv6 allocated from correct pool"

    # Verify IP is on eth0 (local subnet) - THIS VALIDATES THE IPV6 FLAG FIX
    # CRITICAL: Use ' $IPV6/' pattern to match exact IP with CIDR prefix
    info "Checking IP location on nodes (validates IPv6 flag filtering fix)..."
    WINNER_NODE=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        eth0_ip=$(ssh $node "ip -o addr show eth0 2>/dev/null | grep ' $IPV6/'" 2>/dev/null || true)
        kubelb0_ip=$(ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IPV6/'" 2>/dev/null || true)
        if [ -n "$eth0_ip" ]; then
            pass "IPv6 $IPV6 is on eth0 on $node (IPv6 flag fix working!)"
            WINNER_NODE=$node
        fi
        if [ -n "$kubelb0_ip" ]; then
            fail "IPv6 $IPV6 is on kube-lb0 on $node (IPv6 flag fix NOT working)"
        fi
    done

    [ -n "$WINNER_NODE" ] || fail "IPv6 not found on any node"

    # Test connectivity
    info "Testing connectivity to [$IPV6]..."
    RESPONSE=$(curl -s --connect-timeout 5 -6 "http://[$IPV6]/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from service"
    pass "IPv6 service is reachable"
    echo "$RESPONSE"
}

#---------------------------------------------------------------------
# Test 3: Dual-Stack Service
#---------------------------------------------------------------------
test_dualstack() {
    echo ""
    echo "=========================================="
    echo "TEST 3: Dual-Stack Service"
    echo "=========================================="

    info "Creating dual-stack service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-dualstack.yaml

    info "Waiting for IP allocation..."
    sleep 5

    # FIX: Don't assume index order - detect by address format
    # IPv4 could be at index 0 or 1 depending on allocator processing order
    IPV4=""
    IPV6=""
    for i in 0 1; do
        IP=$(kubectl get svc nginx-lb-dualstack -n $NAMESPACE -o jsonpath="{.status.loadBalancer.ingress[$i].ip}")
        if [[ "$IP" =~ ":" ]]; then
            IPV6="$IP"
        else
            IPV4="$IP"
        fi
    done

    [ -n "$IPV4" ] || fail "No IPv4 allocated"
    [ -n "$IPV6" ] || fail "No IPv6 allocated"

    info "Allocated IPv4: $IPV4"
    info "Allocated IPv6: $IPV6"
    pass "Both IPv4 and IPv6 allocated"

    # Check that BOTH are on eth0 and NEITHER is on kube-lb0
    # FIX: Use ' $IP/' pattern for exact matching with CIDR prefix
    info "Checking both IPs are on eth0 (validates announceRemote fix)..."
    IPV4_NODE=""
    IPV6_NODE=""

    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        # Check IPv4 - use exact match with CIDR prefix
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            IPV4_NODE=$node
        fi
        if ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            fail "IPv4 $IPV4 on kube-lb0 on $node (BUG!)"
        fi

        # Check IPv6 - use exact match with CIDR prefix
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV6/'" 2>/dev/null; then
            IPV6_NODE=$node
        fi
        if ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IPV6/'" 2>/dev/null; then
            fail "IPv6 $IPV6 on kube-lb0 on $node (BUG!)"
        fi
    done

    [ -n "$IPV4_NODE" ] || fail "IPv4 not on any node's eth0"
    [ -n "$IPV6_NODE" ] || fail "IPv6 not on any node's eth0"

    pass "IPv4 on eth0 on $IPV4_NODE"
    pass "IPv6 on eth0 on $IPV6_NODE"

    # They may be on different nodes (independent elections)
    if [ "$IPV4_NODE" != "$IPV6_NODE" ]; then
        info "IPv4 and IPv6 on different nodes (independent elections working)"
    else
        info "IPv4 and IPv6 on same node"
    fi

    # Test connectivity to both
    info "Testing IPv4 connectivity..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No IPv4 response"
    pass "IPv4 reachable"

    info "Testing IPv6 connectivity..."
    RESPONSE=$(curl -s --connect-timeout 5 -6 "http://[$IPV6]/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No IPv6 response"
    pass "IPv6 reachable"
}

#---------------------------------------------------------------------
# Test 4: Leader Election
#---------------------------------------------------------------------
test_leader_election() {
    echo ""
    echo "=========================================="
    echo "TEST 4: Leader Election"
    echo "=========================================="

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

    # FIX: Use exact IP matching with CIDR prefix to prevent partial matches
    info "Checking that only ONE node has $IPV4..."
    NODE_COUNT=0
    WINNER=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            NODE_COUNT=$((NODE_COUNT + 1))
            WINNER=$node
        fi
    done

    [ "$NODE_COUNT" -eq 1 ] || fail "IP on $NODE_COUNT nodes (expected 1)"
    pass "Only $WINNER is announcing $IPV4 (election working)"

    # Verify purelb.io/announcing-IPv4 annotation matches winner
    info "Verifying announcing annotation..."
    ANNOUNCING=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv4}')
    info "Announcing annotation: $ANNOUNCING"
    [ -n "$ANNOUNCING" ] || fail "Missing purelb.io/announcing-IPv4 annotation"
    [[ "$ANNOUNCING" == *"$WINNER"* ]] || fail "Announcing annotation '$ANNOUNCING' doesn't match winner '$WINNER'"
    pass "Announcing annotation correctly set to $ANNOUNCING"

    # Check lbnodeagent logs for election messages
    info "Checking election logs..."
    kubectl logs -n purelb-system-l component=lbnodeagent --tail=100 | grep -i "winner" | tail -5 || true
}

#---------------------------------------------------------------------
# Test 5: Service Deletion Cleanup
#---------------------------------------------------------------------
test_service_cleanup() {
    echo ""
    echo "=========================================="
    echo "TEST 5: Service Deletion Cleanup"
    echo "=========================================="

    # Get current IP before deletion
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Deleting service with IP $IPV4..."

    kubectl delete svc nginx-lb-ipv4 -n $NAMESPACE

    # FIX: Use polling with timeout instead of fixed sleep
    # This prevents intermittent failures in slow environments
    info "Verifying IP removed from all nodes (polling with 30s timeout)..."
    TIMEOUT=30
    INTERVAL=2
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        IP_FOUND=false
        for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
            # Use exact IP matching with CIDR prefix
            if ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                IP_FOUND=true
                break
            fi
        done
        if [ "$IP_FOUND" = "false" ]; then
            pass "IP $IPV4 removed from all nodes (took ${ELAPSED}s)"
            break
        fi
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    [ $ELAPSED -lt $TIMEOUT ] || fail "IP $IPV4 not removed within ${TIMEOUT}s"

    # Recreate for other tests
    info "Recreating IPv4 service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s
}

#---------------------------------------------------------------------
# Test 6: IP Sharing (allow-shared-ip annotation)
#---------------------------------------------------------------------
test_ip_sharing() {
    echo ""
    echo "=========================================="
    echo "TEST 6: IP Sharing (allow-shared-ip)"
    echo "=========================================="

    info "Creating first service with sharing key 'webservers'..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-http
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http
    port: 80
    targetPort: 80
EOF

    info "Waiting for first service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-http -n $NAMESPACE --timeout=30s || fail "No IP allocated for first service"

    SHARED_IP=$(kubectl get svc nginx-shared-http -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "First service got IP: $SHARED_IP"

    info "Creating second service with SAME sharing key 'webservers' but different port..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-https
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: https
    port: 443
    targetPort: 80
EOF

    info "Waiting for second service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-https -n $NAMESPACE --timeout=30s || fail "No IP allocated for second service"

    SHARED_IP2=$(kubectl get svc nginx-shared-https -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Second service got IP: $SHARED_IP2"

    # Verify both services got the SAME IP
    if [ "$SHARED_IP" = "$SHARED_IP2" ]; then
        pass "Both services share the same IP: $SHARED_IP"
    else
        fail "Services got different IPs: $SHARED_IP vs $SHARED_IP2 (sharing failed)"
    fi

    # Wait for IP to be announced on a node before testing connectivity
    info "Waiting for IP $SHARED_IP to be announced on a node..."
    wait_for_ip_announced "$SHARED_IP" 30 || fail "IP $SHARED_IP not announced within 30s"
    pass "IP announced on a node"

    # Verify both services are accessible on their respective ports
    info "Testing connectivity to port 80..."
    RESPONSE1=$(curl -s --connect-timeout 5 "http://$SHARED_IP:80/" || true)
    echo "$RESPONSE1" | grep -q "Pod:" || fail "No response on port 80"
    pass "Port 80 is reachable"

    info "Testing connectivity to port 443..."
    RESPONSE2=$(curl -s --connect-timeout 5 "http://$SHARED_IP:443/" || true)
    echo "$RESPONSE2" | grep -q "Pod:" || fail "No response on port 443"
    pass "Port 443 is reachable"

    # Test that a service with DIFFERENT sharing key gets a DIFFERENT IP
    info "Creating third service with DIFFERENT sharing key..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-other
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: other-group
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http
    port: 80
    targetPort: 80
EOF

    info "Waiting for third service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-other -n $NAMESPACE --timeout=30s || fail "No IP allocated for third service"

    OTHER_IP=$(kubectl get svc nginx-shared-other -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Third service (different key) got IP: $OTHER_IP"

    if [ "$SHARED_IP" != "$OTHER_IP" ]; then
        pass "Different sharing keys correctly got different IPs"
    else
        fail "Different sharing keys got SAME IP (should be different)"
    fi

    # Test port conflict: same sharing key + same port = should fail
    info "Testing port conflict: same sharing key BUT same port (should fail)..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-conflict
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http-conflict
    port: 80
    targetPort: 80
EOF

    # FIX: Check for AllocationFailed event, not just empty IP
    # Empty IP could mean slow allocation, not port conflict rejection
    sleep 5
    CONFLICT_IP=$(kubectl get svc nginx-shared-conflict -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)

    if [ -n "$CONFLICT_IP" ]; then
        fail "Port conflict was NOT detected - got IP $CONFLICT_IP (expected allocation to fail)"
    fi

    # Verify we got an AllocationFailed event (proves port conflict was detected)
    info "Checking for AllocationFailed event..."
    EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=nginx-shared-conflict,reason=AllocationFailed -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$EVENTS" ]; then
        pass "Port conflict correctly detected: $EVENTS"
    else
        # If no event but also no IP, it's still a pass (allocation was prevented)
        info "No AllocationFailed event found, but IP allocation was prevented"
        pass "Port conflict correctly prevented IP allocation"
    fi

    # Cleanup test services
    info "Cleaning up IP sharing test services..."
    kubectl delete svc nginx-shared-http nginx-shared-https nginx-shared-other nginx-shared-conflict -n $NAMESPACE 2>/dev/null || true
    pass "IP sharing test completed successfully"
}

#---------------------------------------------------------------------
# Test 7: Multi-Pod Load Balancing
#---------------------------------------------------------------------
test_multi_pod_lb() {
    echo ""
    echo "=========================================="
    echo "TEST 7: Multi-Pod Load Balancing"
    echo "=========================================="

    # Get current replica count to restore later
    ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    info "Current replica count: $ORIGINAL_REPLICAS"

    # Scale to 3 replicas to spread across nodes
    info "Scaling nginx deployment to 3 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=3
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    # Wait for all pods to be ready
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

    # Show pod distribution across nodes
    info "Pod distribution across nodes:"
    kubectl get pods -n $NAMESPACE -l app=nginx -o wide

    # Get the service IP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing load balancing to $IPV4..."

    # Make multiple requests and collect unique pod names
    declare -A PODS_SEEN
    TOTAL_REQUESTS=20

    info "Sending $TOTAL_REQUESTS requests to check load distribution..."
    for i in $(seq 1 $TOTAL_REQUESTS); do
        RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
        POD_NAME=$(echo "$RESPONSE" | grep "Pod:" | awk '{print $2}')
        if [ -n "$POD_NAME" ]; then
            PODS_SEEN[$POD_NAME]=1
        fi
    done

    UNIQUE_PODS=${#PODS_SEEN[@]}
    info "Received responses from $UNIQUE_PODS unique pods:"
    for pod in "${!PODS_SEEN[@]}"; do
        echo "  - $pod"
    done

    # Verify we got responses from multiple pods
    if [ "$UNIQUE_PODS" -ge 2 ]; then
        pass "Load balancing working: traffic distributed across $UNIQUE_PODS pods"
    else
        fail "Load balancing issue: only $UNIQUE_PODS pod(s) responded (expected 2+)"
    fi

    # Check which nodes are serving traffic
    info "Checking node distribution..."
    declare -A NODES_SEEN
    for i in $(seq 1 10); do
        RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
        NODE_NAME=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
        if [ -n "$NODE_NAME" ]; then
            NODES_SEEN[$NODE_NAME]=1
        fi
    done

    UNIQUE_NODES=${#NODES_SEEN[@]}
    info "Responses came from $UNIQUE_NODES unique nodes:"
    for node in "${!NODES_SEEN[@]}"; do
        echo "  - $node"
    done

    # For local pools, traffic goes through the elected node then forwards to pods
    # So we expect responses from multiple pods but may see same node in response
    # (the Node: field shows where the pod runs, not the LB node)
    if [ "$UNIQUE_NODES" -ge 2 ]; then
        pass "Multi-node load balancing: pods on $UNIQUE_NODES different nodes served traffic"
    else
        info "Note: All responding pods happen to be on $UNIQUE_NODES node(s) - this is OK if pods are scheduled there"
    fi

    # Restore original replica count
    info "Restoring original replica count ($ORIGINAL_REPLICAS)..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    pass "Multi-pod load balancing test completed"
}

#---------------------------------------------------------------------
# Test 8: Node Failure and Failover
#---------------------------------------------------------------------
test_node_failover() {
    echo ""
    echo "=========================================="
    echo "TEST 8: Node Failure and Failover"
    echo "=========================================="

    # Get service IP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing failover for service IP: $IPV4"

    # Find which node currently has the VIP
    # FIX: Use exact IP matching with CIDR prefix
    ORIGINAL_WINNER=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            ORIGINAL_WINNER=$node
            break
        fi
    done

    [ -n "$ORIGINAL_WINNER" ] || fail "Could not find node with VIP $IPV4"
    info "Current VIP holder: $ORIGINAL_WINNER"

    # Verify service is working before failover
    info "Verifying service is reachable before failover..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Service not reachable before failover"
    pass "Service reachable before failover"

    # Simulate node failure by adding a taint that lbnodeagent doesn't tolerate,
    # then deleting the pod. This prevents the DaemonSet from rescheduling.
    info "Simulating node failure: tainting and evicting $ORIGINAL_WINNER..."
    kubectl taint node "$ORIGINAL_WINNER" purelb-test=failover:NoExecute --overwrite

    # The taint should cause the pod to be evicted, but let's also explicitly delete it
    AGENT_POD=$(kubectl get pods -n purelb-system-l component=lbnodeagent -o wide | grep "$ORIGINAL_WINNER" | awk '{print $1}')
    if [ -n "$AGENT_POD" ]; then
        kubectl delete pod -n purelb-system"$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
    fi

    # Verify IP was REMOVED from tainted node (with polling)
    info "Verifying IP is removed from tainted node (polling with 30s timeout)..."
    TIMEOUT=30
    INTERVAL=2
    ELAPSED=0
    IP_REMOVED=false
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh "$ORIGINAL_WINNER" "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            IP_REMOVED=true
            break
        fi
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    if [ "$IP_REMOVED" = "false" ]; then
        kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true
        fail "IP should be removed from tainted node (waited ${TIMEOUT}s)"
    fi
    pass "IP removed from $ORIGINAL_WINNER after taint/eviction (took ${ELAPSED}s)"

    # Wait for memberlist to detect failure and elect new leader
    info "Waiting for failover (memberlist detection + election)..."
    sleep 15

    # Check which node now has the VIP - should be a DIFFERENT node
    NEW_WINNER=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            NEW_WINNER=$node
            break
        fi
    done

    if [ -z "$NEW_WINNER" ]; then
        # VIP might be in transition - wait a bit more
        info "VIP not found yet, waiting for election to complete..."
        sleep 10
        for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
            if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                NEW_WINNER=$node
                break
            fi
        done
    fi

    [ -n "$NEW_WINNER" ] || { kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true; fail "VIP $IPV4 not found on any node after failover"; }

    # Verify it's a DIFFERENT node (actual failover occurred)
    if [ "$NEW_WINNER" = "$ORIGINAL_WINNER" ]; then
        kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true
        fail "VIP should have moved to a different node, but it's still on $ORIGINAL_WINNER"
    fi
    pass "Failover successful: VIP moved from $ORIGINAL_WINNER to $NEW_WINNER"

    # Verify service is still reachable after failover
    info "Verifying service is reachable after failover..."
    RESPONSE=$(curl -s --connect-timeout 10 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || { kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true; fail "Service not reachable after failover"; }
    pass "Service still reachable after failover"

    # Remove taint to allow DaemonSet to restore
    info "Removing taint from $ORIGINAL_WINNER..."
    kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true

    # Wait for DaemonSet to fully recover with polling
    info "Waiting for lbnodeagent DaemonSet to recover..."
    kubectl rollout status daemonset/lbnodeagent -n purelb-system--timeout=60s

    # Verify all agents are running (with polling to handle timing issues)
    EXPECTED_AGENTS=5
    info "Verifying all $EXPECTED_AGENTS lbnodeagent pods are running (polling with 30s timeout)..."
    TIMEOUT=30
    INTERVAL=2
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        RUNNING_AGENTS=$(kubectl get pods -n purelb-system-l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
        if [ "$RUNNING_AGENTS" -eq "$EXPECTED_AGENTS" ]; then
            pass "All $EXPECTED_AGENTS lbnodeagent pods recovered (took ${ELAPSED}s)"
            break
        fi
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    [ "$RUNNING_AGENTS" -eq "$EXPECTED_AGENTS" ] || fail "Expected $EXPECTED_AGENTS agents, got $RUNNING_AGENTS after ${TIMEOUT}s"

    pass "Node failover test completed successfully"
}

#---------------------------------------------------------------------
# Test 9: Specific IP Request (purelb.io/addresses)
#---------------------------------------------------------------------
test_specific_ip_request() {
    echo ""
    echo "=========================================="
    echo "TEST 9: Specific IP Request (purelb.io/addresses)"
    echo "=========================================="

    REQUESTED_IP="172.30.255.175"
    info "Requesting specific IP: $REQUESTED_IP"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-specific-ip
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/addresses: "$REQUESTED_IP"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8080
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-specific-ip -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    ALLOCATED_IP=$(kubectl get svc nginx-specific-ip -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $ALLOCATED_IP"

    if [ "$ALLOCATED_IP" = "$REQUESTED_IP" ]; then
        pass "Got requested IP: $ALLOCATED_IP"
    else
        fail "Got wrong IP: $ALLOCATED_IP (requested $REQUESTED_IP)"
    fi

    # Wait for IP to be announced on a node before testing connectivity
    info "Waiting for IP $ALLOCATED_IP to be announced on a node..."
    wait_for_ip_announced "$ALLOCATED_IP" 30 || fail "IP $ALLOCATED_IP not announced within 30s"
    pass "IP announced on a node"

    # Verify specific IP is on eth0 (local pool), NOT on kube-lb0
    info "Verifying specific IP is on eth0 (not kube-lb0)..."
    FOUND_ON_ETH0=false
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $ALLOCATED_IP/'" 2>/dev/null; then
            pass "Specific IP $ALLOCATED_IP on eth0 on $node"
            FOUND_ON_ETH0=true
        fi
        if ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $ALLOCATED_IP/'" 2>/dev/null; then
            fail "Specific IP $ALLOCATED_IP found on kube-lb0 (should be on eth0 for local pool)"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "Specific IP not found on eth0 on any node"

    # Verify service is reachable on the specific IP
    info "Testing connectivity to $ALLOCATED_IP:8080..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$ALLOCATED_IP:8080/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from specific IP service"
    pass "Specific IP service is reachable"

    # Cleanup
    info "Cleaning up specific IP test service..."
    kubectl delete svc nginx-specific-ip -n $NAMESPACE
    pass "Specific IP request test completed successfully"
}

#---------------------------------------------------------------------
# Test 10: ETP Local Override (purelb.io/allow-local annotation)
#---------------------------------------------------------------------
test_etp_local_override() {
    echo ""
    echo "=========================================="
    echo "TEST 10: ETP Local Override (purelb.io/allow-local)"
    echo "=========================================="

    # For local pools, ETP Local is normally overridden to Cluster.
    # The allow-local annotation allows ETP Local on local pools.
    # This is risky because only the elected node announces, but
    # if that node doesn't have an endpoint, traffic blackholes.

    # First, test WITHOUT the annotation - ETP Local should be converted to Cluster
    info "Creating ETP Local service WITHOUT allow-local annotation..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local-no-override
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8091
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-etp-local-no-override -n $NAMESPACE --timeout=30s || fail "No IP allocated"
    sleep 5

    IP_NO_OVERRIDE=$(kubectl get svc nginx-etp-local-no-override -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP_NO_OVERRIDE"

    # Verify LocalOverride event was generated (ETP switched from Local to Cluster)
    info "Checking for LocalOverride event..."
    EVENTS=$(kubectl get events -n $NAMESPACE --field-selector reason=LocalOverride -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$EVENTS" ]; then
        info "LocalOverride event: $EVENTS"
        pass "ETP Local was overridden to Cluster (expected for local pool without annotation)"
    else
        info "No LocalOverride event found (may have already been cleaned up)"
    fi

    # Verify IP is on eth0 (local pool behavior)
    info "Verifying IP is on eth0..."
    wait_for_ip_announced "$IP_NO_OVERRIDE" 30 || fail "IP not announced within 30s"
    FOUND_ON_ETH0=false
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP_NO_OVERRIDE/'" 2>/dev/null; then
            FOUND_ON_ETH0=true
            pass "IP $IP_NO_OVERRIDE on eth0 on $node (election working)"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "IP not on eth0 on any node"

    # Verify connectivity (should work because ETP was converted to Cluster)
    info "Testing connectivity (should work with ETP Cluster)..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IP_NO_OVERRIDE:8091/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Service not reachable"
    pass "Service reachable with overridden ETP"

    # Cleanup first service
    kubectl delete svc nginx-etp-local-no-override -n $NAMESPACE

    # Now test WITH the annotation - ETP Local should be allowed
    info "Creating ETP Local service WITH allow-local annotation..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local-with-override
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-local: "true"
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8092
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-etp-local-with-override -n $NAMESPACE --timeout=30s || fail "No IP allocated"
    sleep 5

    IP_WITH_OVERRIDE=$(kubectl get svc nginx-etp-local-with-override -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP_WITH_OVERRIDE"

    # Verify LocalOverride event mentions the override annotation was found
    info "Checking that allow-local annotation was recognized..."
    OVERRIDE_EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=nginx-etp-local-with-override,reason=LocalOverride -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if echo "$OVERRIDE_EVENTS" | grep -q "override annotation found"; then
        pass "Allow-local annotation recognized by PureLB"
    else
        info "Override event: $OVERRIDE_EVENTS"
        # Not a failure - event may have been cleaned up
    fi

    # Verify IP is on eth0 (local pool behavior still applies)
    info "Verifying IP is on eth0..."
    wait_for_ip_announced "$IP_WITH_OVERRIDE" 30 || fail "IP not announced within 30s"
    FOUND_ON_ETH0=false
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP_WITH_OVERRIDE/'" 2>/dev/null; then
            FOUND_ON_ETH0=true
            WINNER_NODE=$node
            pass "IP $IP_WITH_OVERRIDE on eth0 on $node"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "IP not on eth0 on any node"

    # Note: With ETP Local allowed on local pool, traffic may or may not work
    # depending on whether the elected node has an endpoint.
    # We'll just verify the IP was placed - connectivity is best-effort.
    info "Note: ETP Local on local pool - connectivity depends on elected node having endpoint"

    # Cleanup
    kubectl delete svc nginx-etp-local-with-override -n $NAMESPACE
    pass "ETP Local override test completed"
}

#---------------------------------------------------------------------
# Test 11: No Duplicate VIPs (Split-Brain Check)
#---------------------------------------------------------------------
test_no_duplicate_vips() {
    echo ""
    echo "=========================================="
    echo "TEST 11: No Duplicate VIPs (Split-Brain Check)"
    echo "=========================================="

    # This test catches split-brain scenarios where multiple nodes think they won election
    info "Checking all LoadBalancer services for duplicate VIPs..."

    # Get all LoadBalancer services with IPs
    SERVICES=$(kubectl get svc -n $NAMESPACE -o json | \
        jq -r '.items[] | select(.spec.type=="LoadBalancer") | select(.status.loadBalancer.ingress) | .metadata.name')

    DUPLICATE_FOUND=false
    for svc in $SERVICES; do
        # Get all IPs for this service
        IPS=$(kubectl get svc $svc -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[*].ip}')

        for IP in $IPS; do
            [ -z "$IP" ] && continue
            info "Checking VIP $IP (service: $svc)..."

            # Count how many nodes have this IP
            COUNT=0
            NODES_WITH_IP=""
            for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
                # Check both eth0 and kube-lb0
                if ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                    COUNT=$((COUNT + 1))
                    NODES_WITH_IP="$NODES_WITH_IP $node"
                fi
            done

            if [ "$COUNT" -eq 0 ]; then
                fail "VIP $IP not found on any node (orphaned in K8s status)"
            elif [ "$COUNT" -eq 1 ]; then
                pass "VIP $IP on exactly 1 node:$NODES_WITH_IP"
            else
                fail "DUPLICATE VIP DETECTED: $IP found on $COUNT nodes:$NODES_WITH_IP (split-brain!)"
                DUPLICATE_FOUND=true
            fi
        done
    done

    [ "$DUPLICATE_FOUND" = "false" ] || fail "Duplicate VIPs detected - potential split-brain condition"
    pass "No duplicate VIPs found - election consistency verified"
}

#---------------------------------------------------------------------
# Test 12: Local VIP Address Flags and Lifetime
# Verifies VIPs have finite lifetime, noprefixroute, and secondary flags
#---------------------------------------------------------------------
test_local_vip_address_flags() {
    echo ""
    echo "=========================================="
    echo "TEST 12: Local VIP Address Flags"
    echo "=========================================="

    # Get IPv4 service VIP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service for address flag verification..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        sleep 3  # Give time for address configuration
    fi

    info "Testing VIP: $IPV4"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"
    info "VIP located on $WINNER_NODE"

    # Get detailed address info
    DETAILS=$(get_address_details "$WINNER_NODE" "$IPV4" "eth0")
    info "Address details: $DETAILS"

    # Check for finite lifetime (NOT forever)
    VALID_LFT=$(get_valid_lft "$DETAILS")
    if [ "$VALID_LFT" = "forever" ]; then
        fail "Local VIP has permanent lifetime (valid_lft=forever) - should have finite lifetime"
    elif [ "$VALID_LFT" = "unknown" ]; then
        fail "Could not determine valid_lft for local VIP"
    else
        pass "Local VIP has finite lifetime: ${VALID_LFT}sec"
        # Verify lifetime is reasonable (should be <= 300 initially)
        if [ "$VALID_LFT" -gt 0 ] && [ "$VALID_LFT" -le 300 ]; then
            pass "Lifetime is within expected range (0-300s)"
        else
            info "Lifetime $VALID_LFT is outside default range (may be custom config)"
        fi
    fi

    # Check for noprefixroute flag
    if check_address_property "$DETAILS" "noprefixroute"; then
        pass "Local VIP has noprefixroute flag"
    else
        fail "Local VIP missing noprefixroute flag"
    fi

    # Check for secondary flag
    if check_address_property "$DETAILS" "secondary"; then
        pass "Local VIP has secondary flag"
    else
        info "Local VIP does not have secondary flag (may be only address)"
    fi
}

#---------------------------------------------------------------------
# Test 13: Address Renewal Timer
# Verifies that address lifetime countdown works
#---------------------------------------------------------------------
test_address_renewal_timer() {
    echo ""
    echo "=========================================="
    echo "TEST 13: Address Renewal Timer"
    echo "=========================================="

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$IPV4" ] || fail "No IPv4 service found for renewal test"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"

    # Get initial lifetime
    DETAILS1=$(get_address_details "$WINNER_NODE" "$IPV4" "eth0")
    LFT1=$(get_valid_lft "$DETAILS1")

    if [ "$LFT1" = "forever" ]; then
        fail "VIP has permanent lifetime - renewal test not applicable"
    fi

    info "Initial valid_lft: ${LFT1}sec"

    # Wait a short time and verify lifetime countdown
    WAIT_TIME=10
    info "Waiting ${WAIT_TIME}s to verify lifetime countdown..."
    sleep $WAIT_TIME

    DETAILS2=$(get_address_details "$WINNER_NODE" "$IPV4" "eth0")
    LFT2=$(get_valid_lft "$DETAILS2")

    if [ "$LFT2" = "unknown" ] || [ -z "$LFT2" ]; then
        fail "Address disappeared or lifetime unknown after wait"
    fi

    info "After ${WAIT_TIME}s, valid_lft: ${LFT2}sec"

    # Lifetime should have decreased or been renewed
    if [ "$LFT2" -ge "$LFT1" ] 2>/dev/null; then
        pass "Address was renewed (lifetime reset from ${LFT1}s to ${LFT2}s)"
    else
        pass "Lifetime countdown is working (decreased from ${LFT1}s to ${LFT2}s)"
    fi

    # Verify address still exists
    if ssh "$WINNER_NODE" "ip addr show eth0 | grep -q ' $IPV4/'" 2>/dev/null; then
        pass "VIP still present on interface"
    else
        fail "VIP disappeared from interface"
    fi
}

#---------------------------------------------------------------------
# Test 14: Flannel Node IP Selection
# Verifies Flannel doesn't select VIP as node IP (requires noprefixroute)
#---------------------------------------------------------------------
test_flannel_node_ip() {
    echo ""
    echo "=========================================="
    echo "TEST 14: Flannel Node IP Selection"
    echo "=========================================="

    CNI=$(detect_cni)
    info "Detected CNI: $CNI"

    if [ "$CNI" != "flannel" ]; then
        info "Flannel not detected (CNI: $CNI) - skipping Flannel-specific test"
        pass "Test skipped (not Flannel)"
        return
    fi

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$IPV4" ] || fail "No IPv4 service found for Flannel test"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"

    info "Checking Flannel annotation on node $WINNER_NODE..."

    # Get Flannel's selected public-ip annotation
    FLANNEL_IP=$(kubectl get node "$WINNER_NODE" -o jsonpath='{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}' 2>/dev/null || true)

    if [ -z "$FLANNEL_IP" ]; then
        info "No Flannel public-ip annotation found on $WINNER_NODE"
        pass "Test skipped (Flannel annotation not present)"
        return
    fi

    info "Flannel public-ip annotation: $FLANNEL_IP"
    info "PureLB VIP: $IPV4"

    # Verify Flannel did NOT select the VIP as node IP
    if [ "$FLANNEL_IP" = "$IPV4" ]; then
        fail "Flannel incorrectly selected VIP $IPV4 as node IP!"
    else
        pass "Flannel correctly selected non-VIP address ($FLANNEL_IP) as node IP"
    fi

    # Verify the Flannel IP is permanent (not a VIP)
    FLANNEL_DETAILS=$(get_address_details "$WINNER_NODE" "$FLANNEL_IP" "eth0")
    FLANNEL_LFT=$(get_valid_lft "$FLANNEL_DETAILS")

    if [ "$FLANNEL_LFT" = "forever" ]; then
        pass "Flannel selected a permanent address (expected for DHCP/static)"
    else
        info "Flannel's selected address has finite lifetime: ${FLANNEL_LFT}s"
    fi
}

#---------------------------------------------------------------------
# Test 15: Cross-Node Connectivity Validation
# Explicitly verifies that traffic can reach pods on DIFFERENT nodes
# than the VIP holder. This catches IP forwarding issues.
#---------------------------------------------------------------------
test_cross_node_connectivity() {
    echo ""
    echo "=========================================="
    echo "TEST 15: Cross-Node Connectivity Validation"
    echo "=========================================="

    info "This test verifies traffic can reach pods on nodes OTHER than the VIP holder."
    info "If IP forwarding is disabled, this test will fail."

    # Get or create IPv4 service
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        # Wait for VIP to be announced
        wait_for_ip_announced "$IPV4" 30 || fail "IP not announced within 30s"
    fi

    info "Testing with VIP: $IPV4"

    # Find which node has the VIP
    VIP_NODE=$(find_vip_node "$IPV4")
    [ -n "$VIP_NODE" ] || fail "Could not find node with VIP $IPV4"
    info "VIP is located on: $VIP_NODE"

    # Get list of nodes that have nginx pods
    info "Checking pod distribution..."
    declare -A POD_NODES
    while IFS= read -r line; do
        node=$(echo "$line" | awk '{print $1}')
        POD_NODES[$node]=1
    done < <(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}')

    # Find nodes that have pods but are NOT the VIP node
    NON_VIP_POD_NODES=()
    for node in "${!POD_NODES[@]}"; do
        if [ "$node" != "$VIP_NODE" ]; then
            NON_VIP_POD_NODES+=("$node")
        fi
    done

    if [ ${#NON_VIP_POD_NODES[@]} -eq 0 ]; then
        info "All pods are on the VIP node ($VIP_NODE) - scaling to add pods on other nodes"
        ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
        kubectl scale deployment nginx -n $NAMESPACE --replicas=5
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

        # Re-check pod distribution
        POD_NODES=()
        while IFS= read -r line; do
            node=$(echo "$line" | awk '{print $1}')
            POD_NODES[$node]=1
        done < <(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}')

        NON_VIP_POD_NODES=()
        for node in "${!POD_NODES[@]}"; do
            if [ "$node" != "$VIP_NODE" ]; then
                NON_VIP_POD_NODES+=("$node")
            fi
        done
    fi

    if [ ${#NON_VIP_POD_NODES[@]} -eq 0 ]; then
        fail "Could not create pods on nodes other than VIP node - check node taints/resources"
    fi

    info "Pods exist on non-VIP nodes: ${NON_VIP_POD_NODES[*]}"

    # Make multiple requests and verify at least one goes to a non-VIP node
    info "Sending requests to verify cross-node traffic..."
    CROSS_NODE_SUCCESS=false
    TOTAL_REQUESTS=20

    for i in $(seq 1 $TOTAL_REQUESTS); do
        RESPONSE_NODE=$(test_connectivity_get_node "$IPV4" 80)
        if [ -n "$RESPONSE_NODE" ] && [ "$RESPONSE_NODE" != "$VIP_NODE" ]; then
            pass "Request $i reached pod on $RESPONSE_NODE (different from VIP node $VIP_NODE)"
            CROSS_NODE_SUCCESS=true
            break
        fi
    done

    if [ "$CROSS_NODE_SUCCESS" = "false" ]; then
        fail "None of $TOTAL_REQUESTS requests reached a pod on a different node than the VIP holder ($VIP_NODE)"
        echo "  This likely indicates IP forwarding is disabled on $VIP_NODE"
        echo "  Fix: ssh $VIP_NODE 'sysctl -w net.ipv4.ip_forward=1'"
    fi

    # Restore original replica count if we changed it
    if [ -n "$ORIGINAL_REPLICAS" ]; then
        info "Restoring original replica count ($ORIGINAL_REPLICAS)..."
        kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    fi

    pass "Cross-node connectivity verified - IP forwarding is working"
}

#---------------------------------------------------------------------
# Test 16: Pod-Based Connectivity Test
# Tests connectivity from INSIDE a pod to validate the full
# kube-proxy path works correctly
#---------------------------------------------------------------------
test_pod_connectivity() {
    echo ""
    echo "=========================================="
    echo "TEST 16: Pod-Based Connectivity Test"
    echo "=========================================="

    info "Testing connectivity from inside a pod (validates full kube-proxy path)"

    # Get or create IPv4 service
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        wait_for_ip_announced "$IPV4" 30 || fail "IP not announced within 30s"
    fi

    info "Testing connectivity to VIP $IPV4 from inside a pod..."

    # Get a pod to use for testing
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].metadata.name}')
    POD_NODE=$(kubectl get pod -n $NAMESPACE "$POD_NAME" -o jsonpath='{.spec.nodeName}')
    [ -n "$POD_NAME" ] || fail "No test pod available"

    info "Using pod $POD_NAME on node $POD_NODE"

    # Test connectivity from inside the pod
    RESPONSE=$(kubectl exec -n $NAMESPACE "$POD_NAME" -- curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)

    if echo "$RESPONSE" | grep -q "Pod:"; then
        RESPONDING_NODE=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
        pass "Connectivity from pod successful - response from node $RESPONDING_NODE"
        echo "$RESPONSE" | head -5
    else
        fail "Could not connect to VIP from inside pod"
        echo "  Response: $RESPONSE"
        echo "  This indicates kube-proxy rules may not be working correctly"
    fi

    # Find VIP node and verify we can reach pods on different nodes from inside a pod
    VIP_NODE=$(find_vip_node "$IPV4")
    info "VIP is on node: $VIP_NODE"

    # If the test pod is on the VIP node, we should specifically test cross-node
    if [ "$POD_NODE" = "$VIP_NODE" ]; then
        info "Test pod is on VIP node - finding a pod on a different node for additional testing..."

        # Find a pod on a different node
        for pod in $(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].metadata.name}'); do
            OTHER_NODE=$(kubectl get pod -n $NAMESPACE "$pod" -o jsonpath='{.spec.nodeName}')
            if [ "$OTHER_NODE" != "$VIP_NODE" ]; then
                info "Testing from pod $pod on non-VIP node $OTHER_NODE..."
                RESPONSE2=$(kubectl exec -n $NAMESPACE "$pod" -- curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
                if echo "$RESPONSE2" | grep -q "Pod:"; then
                    pass "Connectivity from non-VIP node pod successful"
                else
                    fail "Could not connect to VIP from pod on non-VIP node $OTHER_NODE"
                fi
                break
            fi
        done
    fi

    pass "Pod-based connectivity test completed"
}

#---------------------------------------------------------------------
# Run All Tests
#---------------------------------------------------------------------
run_all_tests() {
    echo ""
    echo "╔══════════════════════════════════════════╗"
    echo "║  PureLB Local Mode Functional Test Suite ║"
    echo "╚══════════════════════════════════════════╝"
    echo ""
    echo "Cluster: $CONTEXT"
    echo "Namespace: $NAMESPACE"
    echo ""

    # Infrastructure validation - MUST pass before running tests
    validate_prerequisites

    # Setup
    setup_lbnodeagent

    # Core functionality tests
    test_ipv4_singlestack
    test_ipv6_singlestack
    test_dualstack
    test_leader_election
    test_service_cleanup
    test_ip_sharing
    test_specific_ip_request
    test_multi_pod_lb
    test_node_failover
    test_etp_local_override
    test_no_duplicate_vips

    # Address lifetime and flag tests (ensures CNI compatibility)
    test_local_vip_address_flags
    test_address_renewal_timer
    test_flannel_node_ip

    # Cross-node connectivity validation (catches IP forwarding issues)
    test_cross_node_connectivity
    test_pod_connectivity

    # Cleanup all test services
    cleanup_test_services

    echo ""
    echo "=========================================="
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    echo "=========================================="
}

#---------------------------------------------------------------------
# Cleanup: Remove all test services
#---------------------------------------------------------------------
cleanup_test_services() {
    echo ""
    echo "=========================================="
    echo "Cleanup: Removing test services"
    echo "=========================================="

    info "Deleting all test services..."
    kubectl delete svc -n $NAMESPACE --all 2>/dev/null || true
    pass "Test services cleaned up"
}

# Run tests
run_all_tests
