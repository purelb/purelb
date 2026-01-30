---
title: "Installation"
description: "Describe Operation"
weight: 10
hide: [ "toc", "footer" ]
---

We recommend that you use [Helm](https://helm.sh/) to install PureLB on production systems. Before you do, though, you need to prepare your k8s cluster.

## Prepare the Cluster
Before installing PureLB, your k8s cluster should be up and running with an operating [Container Network Interface](https://www.redhat.com/sysadmin/cni-kubernetes).

### Firewall Rules
PureLB uses a library called [Memberlist](https://github.com/hashicorp/memberlist) to provide faster local network address failover than k8s does.  If you plan to use local network addresses and have firewalls on your nodes, you must add a rule to allow the memberlist election to occur. PureLB uses both TCP and UDP on port **7934**, so open both.

{{% notice danger %}}f
If UDP/TCP 7934 is not open and a local network address is allocated, PureLB will exhibit "split brain" behavior.  Each node will attempt to allocate the address where the local network addresses match and update v1/service.  This will cause the v1/service to continously update, the LBNodeAgent logs will show repeated attempts to register addresses, and it will appear that PureLB is unstable.
{{% /notice %}}

### ARP Behavior
We recommend that you change the Linux kernel's ARP behavior from its default.  This is necessary if you're using kubeproxy in IPVS mode and is also good security practice. By default Linux will answer ARP requests for addresses on any interface irrespective of the source. It does this to increase the the chance of successful communication, but we recommend changing this setting so Linux only answers ARP requests for addresses on the interface on which it receives the request. This can be done in sysconfig or in the kubeproxy configuration.

Updating the kubeproxy configuration is dependent upon the Kubernetes packaging in use, so please refer to your distribution packaging information.  The following should be used to set IPVS and ARP behavior.

Kubeproxy Configuration

```plaintext
--proxy-mode IPVS
--ipvs-strict-arp
```

Sysctl configuration
```sh
$ cat <<EOF | sudo tee /etc/sysctl.d/k8s_arp.conf
net.ipv4.conf.all.arp_ignore=1
net.ipv4.conf.all.arp_announce=2

EOF
$ sudo sysctl --system
```
{{% notice danger %}}
PureLB will operate without making this change, however if kubeproxy is set to IPVS mode and ARP changes are not made, all nodes will respond to locally allocated addresses as kubeproxy adds these addresses to kube-ipvs0, the behavior is the same as duplicate IP addresses on the same subnet.
{{% /notice %}}

## Install PureLB

### Option 1: Helm Repository

```sh
$ helm repo add purelb https://purelb.github.io/purelb/charts
$ helm repo update
$ helm install --create-namespace --namespace=purelb-system purelb purelb/purelb
```

### Option 2: OCI Registry (Helm 3.8+)

```sh
$ helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.14.0
```

### Option 3: Direct URL

```sh
$ helm install --create-namespace --namespace=purelb-system purelb \
    https://github.com/purelb/purelb/releases/download/v0.14.0/purelb-v0.14.0.tgz
```

Several options can be overridden during installation. See [the chart's values.yaml file](https://github.com/purelb/purelb/blob/main/build/helm/purelb/values.yaml) for the complete set.

For example, if you would like to add a toleration to the allocator deployment (so the allocator can run on tainted nodes) you can create a file called `tolerations.yaml` with the following contents:

```yaml
---
allocator:
  tolerations:
  - effect: NoSchedule
    key: node-role.kubernetes.io/master
```

You can then tell helm to use this file to override PureLB's defaults:

```sh
$ helm install --values=toleration-test.yaml --create-namespace --namespace=purelb-system purelb purelb/purelb
```

### GARP
PureLB supports gratuitous ARP (GARP) which is required for EVPN/VXLAN environments. GARP is disabled by default but can be enabled during installation by setting the `lbnodeagent.sendgarp` flag in the LBNodeAgent configuration. If you're using Helm, then add `--set=lbnodeagent.sendgarp=true` to the command line:

```sh
$ helm install --create-namespace --namespace=purelb-system --set=lbnodeagent.sendgarp=true purelb purelb/purelb

```
It can also be enabled after installation by editing the LBNodeAgent resource:

``` yaml
$ kubectl edit -n purelb-system lbnodeagent
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  ...
spec:
  local:
    extlbint: kube-lb0
    localint: default
    sendgarp: true            # enable GARP

```

### Install from Source
Installation from source isn't recommended for production systems but it's useful for development. The process is covered in the [PureLB readme](https://github.com/purelb/purelb).

## Installed Components
1. PureLB Namespace.  The `purelb-system` namespace is created for the PureLB components
1. Custom Resource Definition.  PureLB uses two CRD's for configuration: `ServiceGroup` and `LBNodeAgent`
1. Allocator Deployment.  A Deployment with a single instance of the Allocator is installed
1. LBNodeAgent Daemonset.  LBNodeAgent runs on all nodes
1. Sample/Default configuration.  The default LBNodeAgent configuration and one sample ServiceGroup are added

## Verify Installation
One instance of the Allocator pod should be running, and an instance of the LBNodeAgent pod should be running on each untainted node.

```sh
$ kubectl get pods --namespace=purelb-system --output=wide
NAME                        READY   STATUS    RESTARTS   AGE     IP               NODE        NOMINATED NODE   READINESS GATES
allocator-5cb95b946-5wmsz   1/1     Running   1          5h28m   10.129.3.152     purelb2-4   <none>           <none>
lbnodeagent-5689z           1/1     Running   2          5h28m   172.30.250.101   purelb2-3   <none>           <none>
lbnodeagent-86nlz           1/1     Running   3          5h27m   172.30.250.104   purelb2-1   <none>           <none>
lbnodeagent-f2cmb           1/1     Running   2          5h27m   172.30.250.103   purelb2-2   <none>           <none>
lbnodeagent-msrgs           1/1     Running   1          5h28m   172.30.250.105   purelb2-5   <none>           <none>
lbnodeagent-wrvrs           1/1     Running   1          5h27m   172.30.250.102   purelb2-4   <none>           <none>
```
