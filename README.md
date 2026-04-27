# PureLB - is a Service Load Balancer for Kubernetes

[PureLB](https://purelb.io) is a load-balancer orchestrator for  [Kubernetes](https://kubernetes.io) clusters. It uses standard
Linux networking, integrates goBGP for routing, and works with the operating
systems netlink library to add and remove address from interfaces to announce service addresses.

## Documentation

**This documentation is not for this version of Purelb.  There is an older version of Purelb at GitLab.**
**The best resource for configuring this version of purelb is the samples until the documentation is updated**

https://purelb.io/

## Quick Start


The default installation includes [k8gobgp](https://github.com/purelb/k8gobgp) as a sidecar for BGP route announcement. After installing, apply a `BGPConfiguration` CR to configure BGP peering (see [sample](deployments/samples-with-gobgp/sample-bgpconfig.yaml)). If you don't need BGP, use the `-nobgp` manifest variants or set `gobgp.enabled=false` in Helm.

### Option 1: Simple Manifest

Install CRDs first, then apply the main install manifest:

```sh
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-v0.16.3.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-v0.16.3.yaml
```

Without BGP support:

```sh
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-crds-nobgp-v0.16.3.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.3/install-nobgp-v0.16.3.yaml
```

The CRDs must be applied first because the install manifest includes a default
`LBNodeAgent` custom resource that requires its CRD to be registered.

### Option 2: Helm (Recommended for Production)

Install PureLB using Helm for more configuration options:

```sh
helm repo add purelb https://purelb.github.io/purelb/charts
helm install --create-namespace --namespace=purelb-system purelb purelb/purelb
```

Or using OCI registry (Helm 3.8+, `--version` required):

```sh
helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.16.3
```

To install without BGP support:

```sh
helm install --create-namespace --namespace=purelb-system purelb purelb/purelb \
    --set gobgp.enabled=false
```

For detailed installation and configuration, see https://purelb.github.io/purelb/install/

### Testing Your Installation

PureLB needs a `ServiceGroup` that tells it which addresses to allocate. The
`deployments/samples` directory contains a working local-pool example with
both IPv4 and IPv6 ranges:
[deployments/samples/local-servicegroup.yaml](deployments/samples/local-servicegroup.yaml).

The sample subnets (`192.168.254.0/24` and `fd53:9ef0:8683::/120`) will almost
certainly **not** be routable on your network. Copy the manifest below into
`servicegroup.yaml`, replace the `subnet` and `pool` values with a range that
is free on your LAN, and apply it:

```yaml
---
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 192.168.254.0/24            # <-- edit: your LAN subnet
      pool: 192.168.254.230-192.168.254.240  # <-- edit: free range in that subnet
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120        # <-- edit: your IPv6 subnet (omit this block on IPv4-only clusters)
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
```

```shell
kubectl apply -f servicegroup.yaml
```

Deploy nginx as a test backend — its default image listens on both IPv4 and
IPv6:

```shell
kubectl create deployment nginx --image=nginx
```

Expose it with a dual-stack `LoadBalancer` Service. The sample
[deployments/samples/sample-nginx-lb.yaml](deployments/samples/sample-nginx-lb.yaml)
uses `ipFamilyPolicy: PreferDualStack`, so it works unchanged on both
single-stack and dual-stack clusters:

```shell
kubectl apply -f https://raw.githubusercontent.com/purelb/purelb/main/deployments/samples/sample-nginx-lb.yaml
```

The PureLB allocator picks one address per enabled family from the pool and
writes them to the Service status. The PureLB node agents elect a winning
node per address and configure the local OS to advertise it.

Verify the allocation:

```shell
kubectl get svc nginx
```

You should see one or two addresses in the `EXTERNAL-IP` column depending on
whether your cluster is single- or dual-stack. From a host with a route to
the pool subnet:

```shell
curl      http://<EXTERNAL-IPv4>
curl -6   http://[<EXTERNAL-IPv6>]
```

For deeper visibility — which node won the election, pool utilization, node
agent health — install the `kubectl-purelb` plugin described below and run:

```shell
kubectl purelb status
kubectl purelb pools
```

### kubectl-purelb Plugin (optional)

The `kubectl-purelb` plugin provides operational visibility commands
for PureLB clusters: pool utilization, service status, election state,
BGP sessions, data plane health, and configuration validation.

Download the binary for your platform from the
[latest release](https://github.com/purelb/purelb/releases/latest)
and place it in your PATH:

**Linux (amd64):**
```shell
curl -LO https://github.com/purelb/purelb/releases/latest/download/kubectl-purelb-linux-amd64
chmod +x kubectl-purelb-linux-amd64
sudo mv kubectl-purelb-linux-amd64 /usr/local/bin/kubectl-purelb
```

**macOS (Apple Silicon):**
```shell
curl -LO https://github.com/purelb/purelb/releases/latest/download/kubectl-purelb-darwin-arm64
chmod +x kubectl-purelb-darwin-arm64
sudo mv kubectl-purelb-darwin-arm64 /usr/local/bin/kubectl-purelb
```

To build the plugin from source, see [BUILDING.md](BUILDING.md).

**Verify and use:**
```shell
kubectl purelb version
kubectl purelb status
kubectl purelb pools
kubectl purelb bgp sessions
```

## Building

PureLB is built with `make` (container images via [ko](https://ko.build/)
under the hood). See [BUILDING.md](BUILDING.md) for Makefile targets,
CI/CD, running PureLB locally against a test cluster, and building images
to your own registry or to a tarball.

## Credits

PureLB was forked from MetalLB in 2020.  We believed a better solution was to use Linux networking functionality instead of working around it but the maintainers at the time had no interest in making any changes.  We would like to acknowledge the original developer, [Dave Anderson](https://www.dave.tf/) we hope you would be pleased with our work!!
