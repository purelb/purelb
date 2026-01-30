# Router Test Setup

## Quick Start

### Basic Version (No Router CLI)
```bash
./test-router-connectivity.sh
```

### FRR Version (With Route Verification)
```bash
export ROUTER_HOST="your-frr-router"
./test-router-connectivity-frr.sh
```

## Architecture Overview

```
This Host (workstation)
    |
    | curl to VIPs
    v
FRR Router  <-- ROUTER_HOST (FRR version only)
    |
    | BGP routes
    v
Cluster Nodes (GoBGP)
    purelb1, purelb2, ...
```

- **Nodes run GoBGP**: Integrated into lbnodeagent, announces VIPs
- **External router is FRR**: Receives BGP routes from nodes
- **Tests run locally**: curl commands run from where the script runs

## ServiceGroup Configuration

The tests use the existing `remote` ServiceGroup from `../remote/servicegroup-remote.yaml`:

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: remote
  namespace: purelb-system
spec:
  local:
    v4pools:
    - aggregation: /32
      pool: 10.255.0.100-10.255.0.150
      subnet: 10.255.0.0/24
```

To use a different ServiceGroup:
```bash
export SERVICE_GROUP="my-custom-group"
```

## FRR Router Configuration

Example FRR configuration for peering with cluster nodes:

```
router bgp 65000
 bgp router-id 10.0.0.1
 no bgp ebgp-requires-policy

 ! Peer with each cluster node running GoBGP
 neighbor 172.30.255.1 remote-as 64512
 neighbor 172.30.255.2 remote-as 64512
 neighbor 172.30.255.3 remote-as 64512

 address-family ipv4 unicast
  neighbor 172.30.255.1 activate
  neighbor 172.30.255.2 activate
  neighbor 172.30.255.3 activate

  ! Enable ECMP
  maximum-paths 8
 exit-address-family
```

## Verification Commands

### Check BGP Sessions (FRR)
```bash
ssh $ROUTER_HOST "vtysh -c 'show bgp summary'"
```

### Check Routes (FRR)
```bash
ssh $ROUTER_HOST "vtysh -c 'show ip route 10.255.0.0/24 longer-prefixes'"
```

### Check VIP on Nodes
```bash
for node in purelb1 purelb2 purelb3; do
  echo "=== $node ==="
  ssh $node "ip -o addr show kube-lb0"
done
```

### Test Connectivity
```bash
curl -s http://10.255.0.100/
```

## All Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ROUTER_HOST` | FRR only | - | FRR router for vtysh |
| `SERVICE_GROUP` | No | `remote` | ServiceGroup name |
| `BGP_CONVERGE_TIMEOUT` | No | `30` | BGP wait timeout (seconds) |
| `ECMP_TEST_REQUESTS` | No | `100` | Requests for ECMP test |
| `VIP_SUBNET` | No | `10.255.0.0/24` | Subnet for FRR queries |
