#!/bin/bash
# migrate-v1-to-v2.sh - Migrate PureLB v1 CRDs to v2 format
#
# Usage:
#   ./migrate-v1-to-v2.sh [options]
#
# Options:
#   --dry-run       Show what would be migrated without making changes
#   --output-dir    Directory to write migrated YAML files (default: ./migrated)
#   --context       kubectl context to use
#   --namespace     Namespace to migrate (default: purelb)
#
# This script exports existing v1 ServiceGroup and LBNodeAgent resources,
# converts them to v2 format, and optionally applies them back to the cluster.

set -euo pipefail

# Defaults
DRY_RUN=false
OUTPUT_DIR="./migrated"
CONTEXT=""
NAMESPACE="purelb"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  --dry-run       Show what would be migrated without making changes"
    echo "  --output-dir    Directory to write migrated YAML files (default: ./migrated)"
    echo "  --context       kubectl context to use"
    echo "  --namespace     Namespace to migrate (default: purelb)"
    echo "  --help          Show this help message"
    exit 1
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --output-dir)
            OUTPUT_DIR="$2"
            shift 2
            ;;
        --context)
            CONTEXT="$2"
            shift 2
            ;;
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        --help)
            usage
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            usage
            ;;
    esac
done

# Build kubectl command with optional context
KUBECTL="kubectl"
if [[ -n "$CONTEXT" ]]; then
    KUBECTL="kubectl --context $CONTEXT"
fi

# Check for required tools
check_dependencies() {
    local missing=()

    if ! command -v jq &> /dev/null; then
        missing+=("jq")
    fi

    if ! command -v kubectl &> /dev/null; then
        missing+=("kubectl")
    fi

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo -e "${RED}Error: Missing required tools: ${missing[*]}${NC}"
        echo "Please install them and try again."
        exit 1
    fi
}

# Convert a v1 ServiceGroup to v2 format
# The key decision is whether to use "local" or "remote" in v2:
# - If the v1 spec has explicit pool configuration, we need user input
# - This script defaults to "local" but warns the user
convert_servicegroup() {
    local input="$1"
    local name
    name=$(echo "$input" | jq -r '.metadata.name')

    echo -e "${YELLOW}Converting ServiceGroup: $name${NC}" >&2

    # Check what type of spec we have
    local has_local has_netbox
    has_local=$(echo "$input" | jq '.spec.local != null')
    has_netbox=$(echo "$input" | jq '.spec.netbox != null')

    if [[ "$has_netbox" == "true" ]]; then
        # Netbox migration is straightforward
        echo "$input" | jq '
            .apiVersion = "purelb.io/v2" |
            del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
                .metadata.generation, .metadata.managedFields) |
            del(.status)
        '
        return
    fi

    if [[ "$has_local" != "true" ]]; then
        echo -e "${RED}  Warning: ServiceGroup $name has no local or netbox spec, skipping${NC}" >&2
        return
    fi

    # Convert local pools
    # v2 separates "local" (same subnet as nodes) from "remote" (different subnet)
    # By default, we migrate to "local" but warn the user
    echo -e "${YELLOW}  Note: Migrating to 'local' pool type. If your addresses are on a${NC}" >&2
    echo -e "${YELLOW}  different subnet than your nodes (using BGP/BIRD), change to 'remote'.${NC}" >&2

    # Use jq with a cleaner approach - save old spec, build new one
    echo "$input" | jq '
        # Save the old local spec
        .spec.local as $old |

        # Update API version
        .apiVersion = "purelb.io/v2" |

        # Build the new v2 local spec
        .spec.local = (
            # Start building new spec
            {}

            # Handle v4pool -> v4Pool
            | if $old.v4pool != null then
                .v4Pool = {
                    pool: $old.v4pool.pool,
                    subnet: $old.v4pool.subnet
                } | if $old.v4pool.aggregation != null and $old.v4pool.aggregation != "" then
                    .v4Pool.aggregation = $old.v4pool.aggregation
                else . end
              elif ($old.v4pools != null) and (($old.v4pools | length) > 0) then
                .v4Pool = {
                    pool: $old.v4pools[0].pool,
                    subnet: $old.v4pools[0].subnet
                } | if $old.v4pools[0].aggregation != null and $old.v4pools[0].aggregation != "" then
                    .v4Pool.aggregation = $old.v4pools[0].aggregation
                else . end
              elif ($old.pool != null) and ($old.subnet != null) and (($old.subnet | contains(":")) | not) then
                .v4Pool = {
                    pool: $old.pool,
                    subnet: $old.subnet
                } | if $old.aggregation != null and $old.aggregation != "" then
                    .v4Pool.aggregation = $old.aggregation
                else . end
              else .
              end

            # Handle v6pool -> v6Pool
            | if $old.v6pool != null then
                .v6Pool = {
                    pool: $old.v6pool.pool,
                    subnet: $old.v6pool.subnet
                } | if $old.v6pool.aggregation != null and $old.v6pool.aggregation != "" then
                    .v6Pool.aggregation = $old.v6pool.aggregation
                else . end
              elif ($old.v6pools != null) and (($old.v6pools | length) > 0) then
                .v6Pool = {
                    pool: $old.v6pools[0].pool,
                    subnet: $old.v6pools[0].subnet
                } | if $old.v6pools[0].aggregation != null and $old.v6pools[0].aggregation != "" then
                    .v6Pool.aggregation = $old.v6pools[0].aggregation
                else . end
              elif ($old.pool != null) and ($old.subnet != null) and ($old.subnet | contains(":")) then
                .v6Pool = {
                    pool: $old.pool,
                    subnet: $old.subnet
                } | if $old.aggregation != null and $old.aggregation != "" then
                    .v6Pool.aggregation = $old.aggregation
                else . end
              else .
              end
        ) |

        # Clean up metadata
        del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields) |
        del(.status)
    '
}

# Convert a v1 LBNodeAgent to v2 format
convert_lbnodeagent() {
    local input="$1"
    local name
    name=$(echo "$input" | jq -r '.metadata.name')

    echo -e "${YELLOW}Converting LBNodeAgent: $name${NC}" >&2

    echo "$input" | jq '
        # Save old local spec
        .spec.local as $old |

        # Update API version
        .apiVersion = "purelb.io/v2" |

        # Build new local spec with renamed fields
        .spec.local = (
            {}
            # localint -> localInterface
            | if $old.localint != null then .localInterface = $old.localint else . end

            # extlbint -> dummyInterface
            | if $old.extlbint != null then .dummyInterface = $old.extlbint else . end

            # Convert sendgarp boolean to garpConfig, or preserve existing garpConfig
            | if $old.garpConfig != null then
                .garpConfig = $old.garpConfig
              elif $old.sendgarp == true then
                .garpConfig = {
                    enabled: true,
                    initialDelay: "100ms",
                    count: 3,
                    interval: "500ms",
                    verifyBeforeSend: true
                }
              else .
              end

            # Preserve addressConfig if present
            | if $old.addressConfig != null then .addressConfig = $old.addressConfig else . end
        ) |

        # Clean up metadata
        del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields) |
        del(.status)
    '
}

# Main migration logic
main() {
    check_dependencies

    echo -e "${GREEN}PureLB v1 to v2 Migration Script${NC}"
    echo ""

    if [[ "$DRY_RUN" == "true" ]]; then
        echo -e "${YELLOW}Running in dry-run mode - no changes will be made${NC}"
        echo ""
    fi

    # Create output directory
    mkdir -p "$OUTPUT_DIR"

    # Export and convert ServiceGroups
    echo -e "${GREEN}=== Migrating ServiceGroups ===${NC}"

    local sg_count=0
    while IFS= read -r sg; do
        if [[ -n "$sg" ]]; then
            local sg_name
            sg_name=$(echo "$sg" | jq -r '.metadata.name')

            converted=$(convert_servicegroup "$sg")

            if [[ -n "$converted" ]]; then
                echo "$converted" > "$OUTPUT_DIR/servicegroup-$sg_name.yaml"
                echo -e "${GREEN}  Written: $OUTPUT_DIR/servicegroup-$sg_name.yaml${NC}"
                ((sg_count++)) || true
            fi
        fi
    done < <($KUBECTL get servicegroups.purelb.io -A -o json 2>/dev/null | jq -c '.items[]' 2>/dev/null || echo "")

    if [[ $sg_count -eq 0 ]]; then
        echo -e "${YELLOW}  No ServiceGroups found${NC}"
    else
        echo -e "${GREEN}  Migrated $sg_count ServiceGroup(s)${NC}"
    fi
    echo ""

    # Export and convert LBNodeAgents
    echo -e "${GREEN}=== Migrating LBNodeAgents ===${NC}"

    local lbna_count=0
    while IFS= read -r lbna; do
        if [[ -n "$lbna" ]]; then
            local lbna_name
            lbna_name=$(echo "$lbna" | jq -r '.metadata.name')

            converted=$(convert_lbnodeagent "$lbna")

            if [[ -n "$converted" ]]; then
                echo "$converted" > "$OUTPUT_DIR/lbnodeagent-$lbna_name.yaml"
                echo -e "${GREEN}  Written: $OUTPUT_DIR/lbnodeagent-$lbna_name.yaml${NC}"
                ((lbna_count++)) || true
            fi
        fi
    done < <($KUBECTL get lbnodeagents.purelb.io -A -o json 2>/dev/null | jq -c '.items[]' 2>/dev/null || echo "")

    if [[ $lbna_count -eq 0 ]]; then
        echo -e "${YELLOW}  No LBNodeAgents found${NC}"
    else
        echo -e "${GREEN}  Migrated $lbna_count LBNodeAgent(s)${NC}"
    fi
    echo ""

    # Summary
    echo -e "${GREEN}=== Migration Summary ===${NC}"
    echo "  ServiceGroups migrated: $sg_count"
    echo "  LBNodeAgents migrated:  $lbna_count"
    echo "  Output directory:       $OUTPUT_DIR"
    echo ""

    if [[ "$DRY_RUN" == "false" && ($sg_count -gt 0 || $lbna_count -gt 0) ]]; then
        echo -e "${YELLOW}Next steps:${NC}"
        echo "  1. Review the migrated YAML files in $OUTPUT_DIR"
        echo "  2. For ServiceGroups, decide if each should use 'local' or 'remote':"
        echo "     - Use 'local' if addresses are on the same subnet as your nodes"
        echo "     - Use 'remote' if addresses are on a different subnet (BGP/BIRD)"
        echo "  3. Apply the v2 CRDs: kubectl apply -f deployments/crds/"
        echo "  4. Apply the migrated resources: kubectl apply -f $OUTPUT_DIR/"
        echo ""
        echo -e "${YELLOW}Important:${NC}"
        echo "  - Back up your existing resources before applying v2 CRDs"
        echo "  - The v2 CRDs will replace v1 - this is a one-way migration"
    fi
}

main "$@"
