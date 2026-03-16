#!/bin/bash
#
# Stress test for graceful failover to find race conditions.
# Runs the failover test multiple times with varied timing parameters.
#
# Features:
# - Multiple VIPs distributed across subnets (election contention per subnet)
# - Dual-stack VIP (verifies IPv4 + IPv6 subnet correctness together)
# - Force kill (--grace-period=0) to test hard crashes
# - Cascading failover (kill new winner immediately)
# - Election noise (random pod deletions during failover)
# - Node tainting (prevents pod rescheduling, simulates true node failure)
# - Subnet-aware validation: VIP must land on correct subnet after failover
# - IPv6 subnet validation: IPv6 VIP holder must be on correct subnet
# - Address flags check: finite lifetime, noprefixroute, secondary
# - Stale VIP cleanup: old node loses VIP after graceful failover
# - Flannel CNI protection: VIP never selected as flannel node IP
# - Cross-subnet pod routing: nginx scaled to all nodes so curl tests L3 routing
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="test"
PURELB_NS="purelb-system"
ITERATIONS=${1:-10}
LOG_DIR="/tmp/failover-stress-$(date +%Y%m%d-%H%M%S)"

# Source shared helpers (node discovery, SSH-by-IP, VIP helpers, etc.)
source "${SCRIPT_DIR}/../common.sh"

mkdir -p "$LOG_DIR"

echo "=========================================="
echo "Failover Stress Test (Enhanced)"
echo "=========================================="
echo "Iterations: $ITERATIONS"
echo "Log directory: $LOG_DIR"
echo "Nodes: $NODES ($NODE_COUNT total)"
echo "Subnets: $SUBNETS ($SUBNET_COUNT total)"
echo ""

# Show subnet topology with node IPs
echo "Subnet topology:"
for s in $SUBNETS; do
    local_nodes="${SUBNET_NODES[$s]}"
    local_count=$(echo "$local_nodes" | wc -w)
    node_list=""
    for n in $local_nodes; do
        node_list="$node_list  $(node_label $n)"
    done
    echo "  $s ($local_count nodes):$node_list"
done
echo ""

echo "Test modes:"
echo "  - Basic failover (graceful)"
echo "  - Force kill (--grace-period=0)"
echo "  - Cascading failover (kill new winner)"
echo "  - Election noise (random pod deletions)"
echo "  - Multiple VIPs (election contention per subnet)"
echo "  - Balanced VIPs (failover with balanced allocation)"
echo "  - Node tainting (prevents pod rescheduling)"
echo ""

# Counters
PASS=0
FAIL=0
TOTAL=0

# Track created services and ServiceGroups for cleanup
EXTRA_SERVICES=()
STRESS_SGS=()

get_pod_on_node() {
    local NODE=$1
    kubectl get pods -n $PURELB_NS -o wide 2>/dev/null | grep lbnodeagent | grep "$NODE" | awk '{print $1}'
}

capture_state() {
    local LABEL=$1
    local LOGFILE=$2

    echo "=== $LABEL ===" >> "$LOGFILE"
    echo "Timestamp: $(date)" >> "$LOGFILE"
    echo "" >> "$LOGFILE"

    echo "--- Leases ---" >> "$LOGFILE"
    kubectl get leases -n $PURELB_NS 2>/dev/null >> "$LOGFILE" || true
    echo "" >> "$LOGFILE"

    echo "--- Pods ---" >> "$LOGFILE"
    kubectl get pods -n $PURELB_NS -o wide 2>/dev/null >> "$LOGFILE" || true
    echo "" >> "$LOGFILE"

    echo "--- VIP Locations (IPv4) ---" >> "$LOGFILE"
    for node in $NODES; do
        printf "%-55s " "$(node_label $node) [${NODE_SUBNET[$node]}]:" >> "$LOGFILE"
        node_ssh "$node" "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -E 'inet ' | grep -v '${NODE_IPS[$node]}' | awk '{print \$4}' | tr '\n' ' '" 2>/dev/null >> "$LOGFILE" || printf "unreachable" >> "$LOGFILE"
        echo "" >> "$LOGFILE"
    done
    echo "" >> "$LOGFILE"

    echo "--- VIP Locations (IPv6) ---" >> "$LOGFILE"
    for node in $NODES; do
        printf "%-55s " "$(node_label $node) [${NODE_SUBNET[$node]}]:" >> "$LOGFILE"
        node_ssh "$node" "ip -6 -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep 'deprecated' | awk '{print \$4}' | tr '\n' ' '" 2>/dev/null >> "$LOGFILE" || printf "unreachable" >> "$LOGFILE"
        echo "" >> "$LOGFILE"
    done
    echo "" >> "$LOGFILE"
}

create_election_noise() {
    local VIP_HOLDER=$1
    local LOGFILE=$2
    local NOISE_DURATION=$3

    echo "=== NOISE GENERATOR STARTED ===" >> "$LOGFILE"

    local END_TIME=$(($(date +%s) + NOISE_DURATION))

    while [ $(date +%s) -lt $END_TIME ]; do
        local ALL_NODES=($NODES)
        local CANDIDATE=""
        local CANDIDATE_POD=""

        for node in $(echo "${ALL_NODES[@]}" | tr ' ' '\n' | shuf); do
            if [ "$node" != "$VIP_HOLDER" ]; then
                local pod=$(get_pod_on_node "$node")
                if [ -n "$pod" ]; then
                    CANDIDATE=$node
                    CANDIDATE_POD=$pod
                    break
                fi
            fi
        done

        if [ -n "$CANDIDATE" ]; then
            echo "  $(ts) [NOISE] Deleting pod on $(node_label $CANDIDATE)" >> "$LOGFILE"
            kubectl delete pod -n $PURELB_NS "$CANDIDATE_POD" --grace-period=3 >> "$LOGFILE" 2>&1 &
            sleep "0.$((5 + RANDOM % 15))"
        else
            sleep 0.5
        fi
    done

    echo "=== NOISE GENERATOR STOPPED ===" >> "$LOGFILE"
}

# Create multiple VIP services for election contention, distributed across subnets
setup_multiple_vips() {
    echo "Setting up multiple VIPs for election contention..."

    if [ "$SUBNET_COUNT" -ge 2 ]; then
        # Multi-subnet: 2 VIPs per subnet using per-subnet ServiceGroups (.230-.240 range)
        local idx=0
        for s in $SUBNETS; do
            idx=$((idx + 1))
            local sg_name="stress-vips-subnet${idx}"
            STRESS_SGS+=("$sg_name")

            local pool
            pool=$(subnet_test_pool_range "$s")
            local yaml="apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: ${sg_name}
  namespace: purelb-system
spec:
  local:
    v4pools:
    - aggregation: default
      pool: ${pool}
      subnet: ${s}"
            local v6sub="${SUBNET_V6[$s]}"
            local v6prefix="${SUBNET_V6_PREFIX[$s]}"
            if [ -n "$v6sub" ]; then
                local v6pool
                v6pool=$(v6_test_pool_range "$v6prefix")
                yaml="${yaml}
    v6pools:
    - aggregation: default
      pool: ${v6pool}
      subnet: ${v6sub}"
            fi
            echo "$yaml" | kubectl apply -f - 2>/dev/null
            echo "  ServiceGroup $sg_name -> $s (${pool})"

            for j in 1 2; do
                local svc_name="nginx-lb-stress-${idx}-${j}"
                EXTRA_SERVICES+=("$svc_name")
                cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $svc_name
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: $sg_name
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies: [IPv4]
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF
            done
        done
    else
        # Single subnet: 4 extra VIPs from default pool
        for i in 2 3 4 5; do
            local svc_name="nginx-lb-stress-$i"
            EXTRA_SERVICES+=("$svc_name")
            cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $svc_name
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies: [IPv4]
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF
        done
    fi

    # Add balanced allocation VIPs (2 services from a balanced SG)
    # Uses .170-.179 / :e::1-:e::10 (non-overlapping with other test ranges)
    if [ "$SUBNET_COUNT" -ge 2 ]; then
        echo "Setting up balanced allocation VIPs..."
        local bal_sg="stress-balanced"
        STRESS_SGS+=("$bal_sg")

        local bal_v4pools=""
        local bal_v6pools=""
        for s in $SUBNETS; do
            if echo "$s" | grep -q ':'; then continue; fi
            local net="${s%/*}"
            local prefix
            prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
            bal_v4pools="${bal_v4pools}
    - pool: ${prefix}.170-${prefix}.179
      subnet: ${s}"

            local v6prefix="${SUBNET_V6_PREFIX[$s]}"
            if [ -n "$v6prefix" ]; then
                local v6sub="${SUBNET_V6[$s]}"
                local v6base="${v6prefix%%::}"
                bal_v6pools="${bal_v6pools}
    - pool: ${v6base}:e::1-${v6base}:e::10
      subnet: ${v6sub}"
            fi
        done

        local bal_spec="balanced: true
    v4pools: ${bal_v4pools}"
        [ -n "$bal_v6pools" ] && bal_spec="${bal_spec}
    v6pools: ${bal_v6pools}"

        kubectl apply -f - <<EOF 2>/dev/null
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: ${bal_sg}
  namespace: purelb-system
spec:
  local:
    ${bal_spec}
EOF
        echo "  ServiceGroup $bal_sg -> balanced across $SUBNET_COUNT subnets"

        for j in 1 2; do
            local svc_name="nginx-lb-stress-bal-${j}"
            EXTRA_SERVICES+=("$svc_name")
            cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $svc_name
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: $bal_sg
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies: [IPv4]
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF
        done
    fi

    echo "Waiting for VIPs to be allocated..."
    for svc in "${EXTRA_SERVICES[@]}"; do
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/$svc -n $NAMESPACE --timeout=30s 2>/dev/null || true
    done

    sleep 3
    echo "Multiple VIPs ready:"
    kubectl get svc -n $NAMESPACE 2>/dev/null | grep "nginx-lb" | while read line; do
        echo "  $line"
    done
}

cleanup_extra_vips() {
    echo "Cleaning up extra VIP services..."
    for svc in "${EXTRA_SERVICES[@]}"; do
        kubectl delete svc "$svc" -n $NAMESPACE 2>/dev/null || true
    done
    for sg in "${STRESS_SGS[@]}"; do
        kubectl delete servicegroup "$sg" -n $PURELB_NS 2>/dev/null || true
    done
    cleanup_servicegroups
}

cleanup_primary_services() {
    echo "Cleaning up primary test services..."
    kubectl delete svc nginx-lb-ipv4 nginx-lb-ipv6 nginx-lb-dualstack \
        -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup default -n $PURELB_NS --ignore-not-found 2>/dev/null || true
}

run_single_test() {
    local ITERATION=$1
    local GRACE_PERIOD=$2
    local WAIT_BEFORE=$3
    local WITH_NOISE=$4
    local CASCADE=$5
    local WITH_TAINT=$6
    local LOGFILE="$LOG_DIR/iteration-${ITERATION}.log"

    local DESC="grace=$GRACE_PERIOD"
    [ "$GRACE_PERIOD" = "0" ] && DESC="FORCE_KILL"
    [ "$WITH_NOISE" = "1" ] && DESC="$DESC +NOISE"
    [ "$CASCADE" = "1" ] && DESC="$DESC +CASCADE"
    [ "$WITH_TAINT" = "1" ] && DESC="$DESC +TAINT"

    echo ""
    echo -e "${BLUE}--- Iteration $ITERATION: $DESC ---${NC}"

    # Get current VIPs
    local IPV4
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    local IPV6=""
    [ "$HAS_IPV6" = "true" ] && IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    local IPV4_DS=""
    local IPV6_DS=""
    if [ "$HAS_IPV6" = "true" ]; then
        IPV4_DS=$(kubectl get svc nginx-lb-dualstack -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        IPV6_DS=$(kubectl get svc nginx-lb-dualstack -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[1].ip}' 2>/dev/null || true)
    fi

    # Collect balanced VIPs
    local BAL_VIPS=()
    local bal_svc_names
    bal_svc_names=$(kubectl get svc -n $NAMESPACE -o name 2>/dev/null | grep "nginx-lb-stress-bal" || true)
    for svc in $bal_svc_names; do
        local bal_ip
        bal_ip=$(kubectl get $svc -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        [ -n "$bal_ip" ] && BAL_VIPS+=("$bal_ip")
    done

    if [ -z "$IPV4" ]; then
        echo -e "${RED}  ERROR: No IPv4 VIP found${NC}"
        echo "ERROR: No IPv4 VIP found" >> "$LOGFILE"
        return 1
    fi

    local ORIGINAL
    ORIGINAL=$(get_vip_holder "$IPV4")
    if [ "$ORIGINAL" = "NONE" ]; then
        echo -e "${RED}  ERROR: VIP not on any node${NC}"
        capture_state "PRE-TEST (VIP MISSING)" "$LOGFILE"
        return 1
    fi

    echo "  IPv4 VIP: $IPV4 on $(node_label $ORIGINAL) [${NODE_SUBNET[$ORIGINAL]}]"
    if [ -n "$IPV6" ]; then
        local V6_INITIAL
        V6_INITIAL=$(get_v6_vip_holder "$IPV6" 2>/dev/null || echo "NONE")
        echo "  IPv6 VIP: $IPV6 on $([ "$V6_INITIAL" != "NONE" ] && node_label $V6_INITIAL || echo "NONE")"
    fi
    if [ -n "$IPV4_DS" ]; then
        echo "  Dual-stack: v4=$IPV4_DS  v6=${IPV6_DS:-none}"
    fi
    if [ ${#BAL_VIPS[@]} -gt 0 ]; then
        echo "  Balanced VIPs: ${BAL_VIPS[*]}"
    fi

    capture_state "PRE-FAILOVER" "$LOGFILE"

    local POD
    POD=$(get_pod_on_node "$ORIGINAL")
    if [ -z "$POD" ]; then
        echo -e "${RED}  ERROR: No pod found on $(node_label $ORIGINAL)${NC}"
        return 1
    fi

    # Subnet of the primary VIP — used for cascade safety check
    local VIP_SUBNET
    VIP_SUBNET=$(ip_to_subnet "$IPV4" 2>/dev/null || echo "")
    local SUBNET_NODE_COUNT=0
    [ -n "$VIP_SUBNET" ] && SUBNET_NODE_COUNT=$(echo "${SUBNET_NODES[$VIP_SUBNET]}" | wc -w)

    # Cascade would exhaust a 2-node subnet — skip it
    local EFFECTIVE_CASCADE=$CASCADE
    if [ "$CASCADE" = "1" ] && [ "$SUBNET_NODE_COUNT" -gt 0 ] && [ "$SUBNET_NODE_COUNT" -lt 3 ]; then
        echo "  $(ts) Skipping cascade on small subnet ($VIP_SUBNET has $SUBNET_NODE_COUNT nodes)"
        EFFECTIVE_CASCADE=0
    fi

    if [ "$WITH_TAINT" = "1" ]; then
        echo "  $(ts) Tainting $(node_label $ORIGINAL) to prevent rescheduling..."
        kubectl taint nodes "$ORIGINAL" stress-test=true:NoSchedule --overwrite >> "$LOGFILE" 2>&1
    fi

    local NOISE_PID=""
    if [ "$WITH_NOISE" = "1" ]; then
        echo "  $(ts) Starting election noise generator..."
        create_election_noise "$ORIGINAL" "$LOGFILE" 20 &
        NOISE_PID=$!
    fi

    if [ "$WAIT_BEFORE" -gt 0 ]; then
        sleep "$WAIT_BEFORE"
    fi

    local LOG_PID=""
    if [ "$GRACE_PERIOD" != "0" ]; then
        kubectl logs -n $PURELB_NS "$POD" -f --tail=0 >> "$LOGFILE" 2>&1 &
        LOG_PID=$!
    fi

    local START
    START=$(date +%s.%N)
    if [ "$GRACE_PERIOD" = "0" ]; then
        echo -e "  $(ts) ${YELLOW}FORCE KILLING${NC} pod $POD on $(node_label $ORIGINAL)..."
    else
        echo "  $(ts) Deleting VIP holder pod $POD on $(node_label $ORIGINAL) (grace=$GRACE_PERIOD)..."
    fi
    kubectl delete pod -n $PURELB_NS "$POD" --grace-period=$GRACE_PERIOD >> "$LOGFILE" 2>&1 &
    local DELETE_PID=$!

    local TIMEOUT=30
    local NEW_HOLDER=""
    local CHECK_INTERVAL=1
    local LOOP_START
    LOOP_START=$(date +%s)
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if [ "$WITH_TAINT" = "1" ]; then
            NEW_HOLDER=$(vip_on_other_node "$IPV4" "$ORIGINAL" 2>/dev/null || true)
        else
            NEW_HOLDER=$(get_vip_holder "$IPV4")
        fi

        ELAPSED=$(( $(date +%s) - LOOP_START ))
        local HOLDER_DESC="${NEW_HOLDER:-NONE}"
        if [ -n "$NEW_HOLDER" ] && [ "$NEW_HOLDER" != "NONE" ]; then
            HOLDER_DESC="$(node_label $NEW_HOLDER) [${NODE_SUBNET[$NEW_HOLDER]}]"
        fi
        echo "  $(ts) [$ELAPSED s] VIP holder: $HOLDER_DESC" >> "$LOGFILE"

        if [ -n "$NEW_HOLDER" ] && [ "$NEW_HOLDER" != "$ORIGINAL" ]; then
            break
        fi

        if [ "$WITH_TAINT" = "0" ] && [ "${NEW_HOLDER:-NONE}" = "$ORIGINAL" ]; then
            local CURRENT_POD
            CURRENT_POD=$(get_pod_on_node "$ORIGINAL")
            if [ -n "$CURRENT_POD" ] && [ "$CURRENT_POD" != "$POD" ]; then
                echo "  $(ts) [$ELAPSED s] New pod $CURRENT_POD replaced $POD on $(node_label $ORIGINAL)" >> "$LOGFILE"
                break
            fi
        fi

        sleep $CHECK_INTERVAL
    done

    wait $DELETE_PID 2>/dev/null || true
    [ -n "$LOG_PID" ] && kill $LOG_PID 2>/dev/null || true

    local FIRST_MOVE_TIME=$ELAPSED
    local FIRST_NEW_HOLDER=${NEW_HOLDER:-NONE}

    # Cascading failover: kill the new winner too
    if [ "$EFFECTIVE_CASCADE" = "1" ] && [ -n "$NEW_HOLDER" ] && [ "$NEW_HOLDER" != "$ORIGINAL" ]; then
        echo -e "  ${CYAN}CASCADE${NC}: Killing new winner $(node_label $NEW_HOLDER) immediately..."
        echo "=== CASCADE: Killing new winner $(node_label $NEW_HOLDER) ===" >> "$LOGFILE"

        local CASCADE_POD
        CASCADE_POD=$(get_pod_on_node "$NEW_HOLDER")
        if [ -n "$CASCADE_POD" ]; then
            kubectl delete pod -n $PURELB_NS "$CASCADE_POD" --grace-period=3 >> "$LOGFILE" 2>&1 &
            local CASCADE_PID=$!

            local CASCADE_TIMEOUT=20
            local CASCADE_ELAPSED=0
            local CASCADE_LOOP_START
            CASCADE_LOOP_START=$(date +%s)
            local SECOND_HOLDER=""
            local SAW_LEAVE=false

            while [ $CASCADE_ELAPSED -lt $CASCADE_TIMEOUT ]; do
                SECOND_HOLDER=$(get_vip_holder "$IPV4")
                CASCADE_ELAPSED=$(( $(date +%s) - CASCADE_LOOP_START ))
                echo "  $(ts) [CASCADE $CASCADE_ELAPSED s] VIP holder: ${SECOND_HOLDER:-NONE}" >> "$LOGFILE"

                if [ "${SECOND_HOLDER:-NONE}" != "$NEW_HOLDER" ]; then
                    SAW_LEAVE=true
                fi

                if [ "$SAW_LEAVE" = true ] && [ -n "$SECOND_HOLDER" ] && [ "$SECOND_HOLDER" != "NONE" ]; then
                    if [ "$WITH_TAINT" = "1" ] && [ "$SECOND_HOLDER" = "$ORIGINAL" ]; then
                        : # tainted node can't run pods
                    else
                        break
                    fi
                fi

                sleep 1
            done

            wait $CASCADE_PID 2>/dev/null || true

            local CASCADE_OK=false
            if [ -n "$SECOND_HOLDER" ] && [ "$SECOND_HOLDER" != "NONE" ]; then
                if [ "$WITH_TAINT" = "1" ] && [ "$SECOND_HOLDER" = "$ORIGINAL" ]; then
                    :
                else
                    CASCADE_OK=true
                fi
            fi

            if [ "$CASCADE_OK" = true ]; then
                if [ "$SECOND_HOLDER" = "$NEW_HOLDER" ]; then
                    echo -e "  ${GREEN}CASCADE OK${NC}: New pod on $(node_label $SECOND_HOLDER) took over"
                elif [ "$SECOND_HOLDER" = "$ORIGINAL" ]; then
                    echo -e "  ${GREEN}CASCADE OK${NC}: Original node $(node_label $SECOND_HOLDER) recovered"
                else
                    echo -e "  ${GREEN}CASCADE OK${NC}: VIP moved to $(node_label $SECOND_HOLDER)"
                fi
                NEW_HOLDER=$SECOND_HOLDER
            else
                echo -e "  ${RED}CASCADE FAIL${NC}: VIP stuck on ${SECOND_HOLDER:-NONE}"
                NEW_HOLDER="NONE"
            fi
        fi
    fi

    [ -n "$NOISE_PID" ] && kill $NOISE_PID 2>/dev/null || true

    if [ "$WITH_TAINT" = "1" ]; then
        echo "  $(ts) Removing taint from $(node_label $ORIGINAL)..."
        kubectl taint nodes "$ORIGINAL" stress-test=true:NoSchedule- >> "$LOGFILE" 2>&1 || true
    fi

    local END
    END=$(date +%s.%N)
    local DURATION
    DURATION=$(echo "$END - $START" | bc)

    capture_state "POST-FAILOVER" "$LOGFILE"

    # Determine final holder
    local FINAL_HOLDER
    if [ "$WITH_TAINT" = "1" ]; then
        FINAL_HOLDER="${NEW_HOLDER:-NONE}"
    else
        FINAL_HOLDER=$(get_vip_holder "$IPV4")
    fi

    local RESULT="UNKNOWN"
    local REACHABLE=false

    if [ "${FINAL_HOLDER:-NONE}" = "NONE" ]; then
        RESULT="FAIL_NO_VIP"
        echo -e "  ${RED}FAIL${NC}: VIP not announced anywhere"
        echo "RESULT: FAIL - VIP not announced on any node" >> "$LOGFILE"
    elif [ "$WITH_TAINT" = "1" ] && [ "$FINAL_HOLDER" = "$ORIGINAL" ]; then
        RESULT="FAIL_NO_MOVE"
        echo -e "  ${RED}FAIL${NC}: VIP stuck on tainted node $(node_label $ORIGINAL)"
        echo "RESULT: FAIL - VIP stuck on tainted node $ORIGINAL" >> "$LOGFILE"
    else
        sleep 1
        if curl -s --connect-timeout 3 "http://$IPV4/" | grep -q "nginx\|Pod:\|Welcome"; then
            REACHABLE=true
        fi

        if [ "$REACHABLE" = true ]; then
            if [ "$FINAL_HOLDER" = "$ORIGINAL" ]; then
                RESULT="PASS_SAME_NODE"
                echo -e "  ${GREEN}PASS${NC}: New pod on $(node_label $ORIGINAL) took over VIP (${DURATION}s)"
                echo "RESULT: PASS - New pod on same node $ORIGINAL" >> "$LOGFILE"
            elif [ "$EFFECTIVE_CASCADE" = "1" ]; then
                RESULT="PASS_CASCADE"
                echo -e "  ${GREEN}PASS${NC}: VIP survived cascade ($(node_label $ORIGINAL) -> $(node_label $FIRST_NEW_HOLDER) -> $(node_label $FINAL_HOLDER))"
                echo "RESULT: PASS - VIP survived cascade to $FINAL_HOLDER" >> "$LOGFILE"
            else
                RESULT="PASS_MOVED"
                echo -e "  ${GREEN}PASS${NC}: VIP moved to $(node_label $FINAL_HOLDER) in ${FIRST_MOVE_TIME}s (total: ${DURATION}s)"
                echo "RESULT: PASS - VIP moved from $ORIGINAL to $FINAL_HOLDER" >> "$LOGFILE"
            fi
            echo -e "  ${GREEN}IPv4 service reachable${NC}"

            # --- SUBNET VALIDATION (IPv4) ---
            if [ "$SUBNET_COUNT" -ge 2 ] && [ "$FINAL_HOLDER" != "$ORIGINAL" ]; then
                if ! verify_vip_subnet_match "$IPV4" "$FINAL_HOLDER" 2>/dev/null; then
                    RESULT="FAIL_WRONG_SUBNET"
                    echo -e "  ${RED}FAIL${NC}: IPv4 VIP $IPV4 on wrong subnet node $(node_label $FINAL_HOLDER) [${NODE_SUBNET[$FINAL_HOLDER]}]"
                    echo "RESULT: FAIL - IPv4 VIP on wrong subnet node $FINAL_HOLDER" >> "$LOGFILE"
                fi
            fi

            # --- IPv6 REACHABILITY AND SUBNET VALIDATION ---
            if [ -n "$IPV6" ] && [[ "$RESULT" == PASS* ]]; then
                local IPV6_OK=false
                for attempt in 1 2 3; do
                    if curl -6 -s --connect-timeout 3 "http://[$IPV6]/" | grep -q "nginx\|Pod:\|Welcome"; then
                        IPV6_OK=true; break
                    fi
                    sleep 1
                done

                if [ "$IPV6_OK" = true ]; then
                    echo -e "  ${GREEN}IPv6 service reachable${NC}"

                    if [ "$SUBNET_COUNT" -ge 2 ]; then
                        local V6_HOLDER
                        V6_HOLDER=$(get_v6_vip_holder "$IPV6" 2>/dev/null || echo "NONE")
                        if [ "$V6_HOLDER" != "NONE" ]; then
                            local V6_NODE_SUBNET="${NODE_SUBNET[$V6_HOLDER]}"
                            local V6_PREFIX="${SUBNET_V6_PREFIX[$V6_NODE_SUBNET]}"
                            if [ -n "$V6_PREFIX" ]; then
                                local v6base="${V6_PREFIX%%::}"
                                if echo "$IPV6" | grep -qi "^${v6base}:"; then
                                    echo "  IPv6 VIP on $(node_label $V6_HOLDER) [correct subnet: $V6_NODE_SUBNET]" | tee -a "$LOGFILE"
                                else
                                    RESULT="FAIL_WRONG_SUBNET_V6"
                                    echo -e "  ${RED}FAIL${NC}: IPv6 VIP $IPV6 on wrong subnet node $(node_label $V6_HOLDER) [$V6_NODE_SUBNET]"
                                    echo "RESULT: FAIL - IPv6 VIP on wrong subnet node $V6_HOLDER" >> "$LOGFILE"
                                fi
                            fi
                        fi
                    fi
                else
                    RESULT="FAIL_IPV6"
                    echo -e "  ${RED}FAIL${NC}: IPv4 OK but IPv6 VIP [$IPV6] not reachable"
                    echo "RESULT: FAIL - IPv6 service not reachable at $IPV6" >> "$LOGFILE"
                fi
            fi

            # --- DUAL-STACK SUBNET VALIDATION ---
            if [ -n "$IPV4_DS" ] && [ "$SUBNET_COUNT" -ge 2 ] && [[ "$RESULT" == PASS* ]]; then
                local DS4_HOLDER
                DS4_HOLDER=$(get_vip_holder "$IPV4_DS" 2>/dev/null || echo "NONE")
                if [ "$DS4_HOLDER" != "NONE" ]; then
                    if ! verify_vip_subnet_match "$IPV4_DS" "$DS4_HOLDER" 2>/dev/null; then
                        RESULT="FAIL_DS_SUBNET"
                        echo -e "  ${RED}FAIL${NC}: Dual-stack IPv4 VIP $IPV4_DS on wrong subnet node $(node_label $DS4_HOLDER)"
                        echo "RESULT: FAIL - Dual-stack IPv4 VIP on wrong subnet node $DS4_HOLDER" >> "$LOGFILE"
                    else
                        echo "  Dual-stack v4 on $(node_label $DS4_HOLDER) [${NODE_SUBNET[$DS4_HOLDER]}] - correct" | tee -a "$LOGFILE"
                    fi
                fi
            fi

            # --- BALANCED VIP SUBNET VALIDATION ---
            if [ ${#BAL_VIPS[@]} -gt 0 ] && [ "$SUBNET_COUNT" -ge 2 ] && [[ "$RESULT" == PASS* ]]; then
                for bal_vip in "${BAL_VIPS[@]}"; do
                    local BAL_HOLDER
                    BAL_HOLDER=$(get_vip_holder "$bal_vip" 2>/dev/null || echo "NONE")
                    if [ "$BAL_HOLDER" = "NONE" ]; then
                        RESULT="FAIL_BAL_MISSING"
                        echo -e "  ${RED}FAIL${NC}: Balanced VIP $bal_vip not announced on any node"
                        echo "RESULT: FAIL - Balanced VIP $bal_vip not on any node" >> "$LOGFILE"
                        break
                    fi
                    if ! verify_vip_subnet_match "$bal_vip" "$BAL_HOLDER" 2>/dev/null; then
                        RESULT="FAIL_BAL_SUBNET"
                        echo -e "  ${RED}FAIL${NC}: Balanced VIP $bal_vip on wrong subnet node $(node_label $BAL_HOLDER) [${NODE_SUBNET[$BAL_HOLDER]}]"
                        echo "RESULT: FAIL - Balanced VIP $bal_vip on wrong subnet node $BAL_HOLDER" >> "$LOGFILE"
                        break
                    fi
                    # Reachability check
                    if ! curl -s --connect-timeout 3 "http://$bal_vip/" | grep -q "nginx\|Pod:\|Welcome"; then
                        RESULT="FAIL_BAL_UNREACHABLE"
                        echo -e "  ${RED}FAIL${NC}: Balanced VIP $bal_vip on $(node_label $BAL_HOLDER) but not reachable"
                        echo "RESULT: FAIL - Balanced VIP $bal_vip unreachable" >> "$LOGFILE"
                        break
                    fi
                    echo "  Balanced VIP $bal_vip on $(node_label $BAL_HOLDER) [${NODE_SUBNET[$BAL_HOLDER]}] - correct & reachable" | tee -a "$LOGFILE"
                done
            fi
        else
            RESULT="FAIL_UNREACHABLE"
            echo -e "  ${RED}FAIL${NC}: VIP on $(node_label $FINAL_HOLDER) but service not reachable"
            echo "RESULT: FAIL - Service not reachable at $IPV4" >> "$LOGFILE"
        fi
    fi

    # --- STALE VIP CHECK: old node should lose VIP after graceful failover ---
    # Skip for tainted tests: after untainting, the DaemonSet recreates the pod and it
    # legitimately re-wins the election — that's correct, not a stale VIP.
    if [[ "$RESULT" == PASS* ]] && [ "$GRACE_PERIOD" != "0" ] && [ "$WITH_TAINT" = "0" ] && \
       [ "$FINAL_HOLDER" != "$ORIGINAL" ] && [ "$FINAL_HOLDER" != "NONE" ]; then
        local STALE_TIMEOUT=15
        local STALE_ELAPSED=0
        while [ $STALE_ELAPSED -lt $STALE_TIMEOUT ]; do
            if ! node_ssh "$ORIGINAL" "ip -o addr show ${NODE_IFACE[$ORIGINAL]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                echo "  Stale VIP cleared from $(node_label $ORIGINAL) in ${STALE_ELAPSED}s" | tee -a "$LOGFILE"
                break
            fi
            sleep 1
            STALE_ELAPSED=$((STALE_ELAPSED + 1))
        done
        if [ $STALE_ELAPSED -ge $STALE_TIMEOUT ]; then
            RESULT="FAIL_STALE_VIP"
            echo -e "  ${RED}FAIL${NC}: Old node $(node_label $ORIGINAL) still has VIP $IPV4 after ${STALE_TIMEOUT}s"
            echo "RESULT: FAIL - Stale VIP on $ORIGINAL after ${STALE_TIMEOUT}s" >> "$LOGFILE"
        fi
    fi

    # --- ADDRESS FLAGS SPOT-CHECK (every other non-tainted iteration) ---
    # Skip for tainted tests: after untainting, the election winner may change before we SSH,
    # causing get_address_details to return empty and falsely report "unknown" lifetime.
    if [[ "$RESULT" == PASS* ]] && [ $((ITERATION % 2)) -eq 0 ] && \
       [ "$WITH_TAINT" = "0" ] && [ "$FINAL_HOLDER" != "NONE" ]; then
        local FLAGS_DETAILS
        FLAGS_DETAILS=$(get_address_details "$FINAL_HOLDER" "$IPV4" "${NODE_IFACE[$FINAL_HOLDER]}" 2>/dev/null || true)
        local LFT
        LFT=$(get_valid_lft "$FLAGS_DETAILS")
        if [ "$LFT" = "forever" ] || [ "$LFT" = "unknown" ]; then
            RESULT="FAIL_BAD_FLAGS"
            echo -e "  ${RED}FAIL${NC}: VIP on $(node_label $FINAL_HOLDER) has invalid lifetime: $LFT (expected finite)"
            echo "RESULT: FAIL - Bad address flags: lifetime=$LFT" >> "$LOGFILE"
        elif ! echo "$FLAGS_DETAILS" | grep -q "noprefixroute"; then
            RESULT="FAIL_BAD_FLAGS"
            echo -e "  ${RED}FAIL${NC}: VIP on $(node_label $FINAL_HOLDER) missing noprefixroute flag"
            echo "RESULT: FAIL - Bad address flags: missing noprefixroute" >> "$LOGFILE"
        else
            echo "  Address flags OK on $(node_label $FINAL_HOLDER): lft=${LFT}s noprefixroute present" | tee -a "$LOGFILE"
        fi
    fi

    # --- FLANNEL NODE IP CHECK ---
    if [[ "$RESULT" == PASS* ]] && [ "$FINAL_HOLDER" != "NONE" ]; then
        local CNI
        CNI=$(detect_cni)
        if [ "$CNI" = "flannel" ]; then
            local FLANNEL_IP
            FLANNEL_IP=$(kubectl get node "$FINAL_HOLDER" \
                -o jsonpath='{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}' 2>/dev/null || true)
            if [ "$FLANNEL_IP" = "$IPV4" ]; then
                RESULT="FAIL_FLANNEL_VIP"
                echo -e "  ${RED}FAIL${NC}: Flannel selected VIP $IPV4 as node IP for $(node_label $FINAL_HOLDER)"
                echo "RESULT: FAIL - Flannel selected VIP as node IP on $FINAL_HOLDER" >> "$LOGFILE"
            else
                echo "  Flannel node IP OK: $(node_label $FINAL_HOLDER) uses $FLANNEL_IP (not VIP)" | tee -a "$LOGFILE"
            fi
        fi
    fi

    # Return based on result
    case $RESULT in
        PASS_*)
            return 0
            ;;
        *)
            echo "" >> "$LOGFILE"
            echo "=== DEBUG: All node logs ===" >> "$LOGFILE"
            for node in $NODES; do
                local npod
                npod=$(get_pod_on_node "$node")
                if [ -n "$npod" ]; then
                    echo "--- $(node_label $node) ($npod) ---" >> "$LOGFILE"
                    kubectl logs -n $PURELB_NS "$npod" --tail=50 2>/dev/null >> "$LOGFILE" || true
                fi
            done
            return 1
            ;;
    esac
}

wait_for_all_pods() {
    local MAX_WAIT=${1:-30}
    for j in $(seq 1 $MAX_WAIT); do
        POD_COUNT=$(kubectl get pods -n $PURELB_NS 2>/dev/null | grep -c "lbnodeagent.*Running" || echo 0)
        if [ "$POD_COUNT" -eq "$NODE_COUNT" ]; then
            return 0
        fi
        sleep 1
    done
    echo -e "  ${YELLOW}WARNING: Only $POD_COUNT/$NODE_COUNT pods running${NC}"
    return 1
}

# Cleanup handler — runs on EXIT (success, failure, or Ctrl-C)
cleanup() {
    echo ""
    echo "Cleaning up..."
    # Remove any lingering taints
    for node in $NODES; do
        kubectl taint nodes "$node" stress-test=true:NoSchedule- 2>/dev/null || true
    done
    # Scale nginx back to 1
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 2>/dev/null || true
    # Clean up extra stress services and ServiceGroups
    cleanup_extra_vips
    # Clean up primary test services and default ServiceGroup
    cleanup_primary_services
    # Kill any remaining background processes
    jobs -p | xargs -r kill 2>/dev/null || true
}
trap cleanup EXIT

# Generate default ServiceGroup (dynamic pools based on discovered subnets)
generate_default_servicegroup

# Ensure test services exist (IPv4 + IPv6 + Dual-Stack)
echo "Ensuring test services exist..."
IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
if [ -z "$IPV4" ]; then
    echo "Creating IPv4 test service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
fi

IPV6=""
if [ "$HAS_IPV6" = "true" ]; then
    IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV6" ]; then
        echo "Creating IPv6 test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv6.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv6 -n $NAMESPACE --timeout=30s
        IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    fi

    # Dual-stack service
    if ! kubectl get svc nginx-lb-dualstack -n $NAMESPACE &>/dev/null; then
        echo "Creating dual-stack test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-dualstack.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-dualstack -n $NAMESPACE --timeout=30s 2>/dev/null || true
    fi
fi

sleep 5  # Wait for announcements
echo "  IPv4 VIP: $IPV4"
echo "  IPv6 VIP: ${IPV6:-(none)}"

# Setup multiple VIPs for election contention
setup_multiple_vips

# Scale nginx to NODE_COUNT replicas — ensures curl tests exercise cross-subnet pod routing
echo "Scaling nginx to $NODE_COUNT replicas for cross-subnet coverage..."
kubectl scale deployment nginx -n $NAMESPACE --replicas=$NODE_COUNT
kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
echo "Pod distribution:"
kubectl get pods -n $NAMESPACE -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName' 2>/dev/null | grep nginx | awk '{printf "  %-45s %s\n", $1, $2}'

echo ""
echo "Starting stress test..."
echo ""

# Run iterations with varied parameters
for i in $(seq 1 $ITERATIONS); do
    TOTAL=$((TOTAL + 1))

    case $((i % 8)) in
        0) GRACE=0  ; CASCADE=0 ; TAINT=0 ;;  # Force kill
        1) GRACE=10 ; CASCADE=0 ; TAINT=0 ;;  # Normal graceful
        2) GRACE=5  ; CASCADE=1 ; TAINT=0 ;;  # Cascade with short grace
        3) GRACE=15 ; CASCADE=0 ; TAINT=1 ;;  # Longer grace + taint
        4) GRACE=0  ; CASCADE=1 ; TAINT=0 ;;  # Force kill + cascade
        5) GRACE=10 ; CASCADE=1 ; TAINT=1 ;;  # Normal + cascade + taint
        6) GRACE=0  ; CASCADE=0 ; TAINT=1 ;;  # Force kill + taint (true node failure)
        7) GRACE=10 ; CASCADE=0 ; TAINT=1 ;;  # Normal + taint
    esac

    WAIT_BEFORE=$((i % 3))

    # Enable noise on ~50% of non-tainted iterations
    WITH_NOISE=$(( TAINT == 0 && (i + 1) % 2 == 0 ? 1 : 0 ))

    if run_single_test $i $GRACE $WAIT_BEFORE $WITH_NOISE $CASCADE $TAINT; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "  Waiting 15s for cluster to stabilize..."
        sleep 15
    fi

    echo "  Waiting for replacement pods..."
    if [ "$CASCADE" = "1" ] || [ "$WITH_NOISE" = "1" ]; then
        sleep 5
        wait_for_all_pods 45
        sleep 3
    else
        sleep 3
        wait_for_all_pods 25
    fi
done

# Summary
echo ""
echo "=========================================="
echo "STRESS TEST RESULTS"
echo "=========================================="
echo "Total iterations: $TOTAL"
echo -e "Passed: ${GREEN}$PASS${NC}"
echo -e "Failed: ${RED}$FAIL${NC}"
echo "Success rate: $(echo "scale=1; $PASS * 100 / $TOTAL" | bc)%"
echo ""
echo "Logs saved to: $LOG_DIR"

if [ $FAIL -gt 0 ]; then
    echo ""
    echo "Failed iterations:"
    grep -l "RESULT: FAIL" "$LOG_DIR"/*.log 2>/dev/null | while read f; do
        echo "  - $(basename $f): $(grep 'RESULT: FAIL' "$f" | head -1)"
    done
    exit 1
fi

exit 0
