---
title: "Install PureLB"
description: "Describe Operation"
weight: 10
hide: toc
---

PureLB can be installed from:

* Helm Chart
* Manifest
* Source repository

## Installed Components
1. PureLB Namespace.  A namespace is created and annotated for all of the PureLB components
2. Custom Resource Definition.  PureLB uses two CRD's for configuration
3. Sample/Default configuration.  The default PureLB configuration and one sample Service Group configuration are added
4. Allocator Deployment.  A deployment with a single instance of the allocator is installed on the cluster
5. lbnodeagent Daemonset.  By default lbnodeagent is installed on all nodes

### Preparing the Cluster
Prior to the installation of PureLB, the k8s cluster should be installed with an operating Container Network Interface.

#### Firewall Rules
PureLB uses a library called Memberlist to provide local network address failover faster than standard k8s timeouts would require.  If you plan to use local network address and have applied firewalls to your nodes, it is necessary to add a rule to allow the memberlist election to occur. The port used by Memberlist in PureLB is **Port 7934 UDP/TCP**, memberlist uses both TCP and UDP, open both.

{{% notice danger %}}
If UDP/TCP 7934 is not open and a local network address is allocated, PureLB will exhibit "split brain" behavior.  Each node will attempt to allocate the address where the local network addresses match and update v1/service.  This will cause the v1/service to continously update, the lbnodeagent logs show repeated attempts to register addresses and it it will appear that PureLB is unstable.
{{% /notice %}}


#### ARP Behavior
{{% notice warning %}}
We recommend that you change the Linux kernel's ARP behavior from its default.  This is necessary if you're using kubeproxy in IPVS mode and is also good security practice.  By default Linux will answer ARP requests for addresses on any interface irrespective of the source. We recommend changing this setting so Linux only answers ARP requests for addresses on the interface it receives the request.  Linux sets this default to increase the the chance of successful communication. This change can be undertaken in sysconfig or in the kubeproxy configuration.  
{{% /notice %}}

Updating the kubeproxy configuration is dependent upon the Kubernetes packaging in use, refer to your distribution packaging information.  The following should be used to set IPVS and ARP behavior.


Kubeproxy Configuration

```plaintext
--proxy-mode IPVS
--ipvs-strict-arp
```

Sysctl configuration
```plaintext
cat <<EOF | sudo tee /etc/sysctl.d/k8s_arp.conf
net.ipv4.conf.all.arp_ignore=1
net.ipv4.conf.all.arp_announce=2

EOF
sudo sysctl --system
```
{{% notice danger %}}
PureLB will operate without making this change, however if kubeproxy is set to IPVS mode and ARP changes are not made, all nodes will respond to locally allocated addresses as kubeproxy adds these addresses to kube-ipvs0, the behavior is the same as duplicate IP addresses on the same subnet.
{{% /notice %}}

### GARP
PureLB supports GARP required for EVPN/VXLAN environments.  GARP can be installed during installation using helm by adding --set=lbnodeagent.sendgarp=true

```plaintext
helm install --create-namespace --namespace=purelb --set=lbnodeagent.sendgarp=true purelb/purelb

```
It can also be enable after installation by editing the LBNodeAgent resources

``` yaml
$ kubectl edit -n purelb lbnodeagent
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  annotations:
    meta.helm.sh/release-name: purelb
    meta.helm.sh/release-namespace: purelb
  creationTimestamp: "2023-04-10T19:57:23Z"
  generation: 1
  labels:
    app.kubernetes.io/instance: purelb
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/name: purelb
    app.kubernetes.io/version: v0.7.0
    helm.sh/chart: purelb-v0.7.0
  name: default
  namespace: purelb
  resourceVersion: "311738"
  uid: 33bf398d-cb8f-416d-9bb1-ed5c12a42a95
spec:
  local:
    extlbint: kube-lb0
    localint: default
    sendgarp: true

```

### Install Using Helm
```plaintext
$ helm repo add purelb https://gitlab.com/api/v4/projects/20400619/packages/helm/stable
$ helm repo update
$ helm install --create-namespace --namespace=purelb purelb purelb/purelb
```

### Install Using the YAML Manifest
A Manifest is simply a concatenated set of yaml files that install all of the components of PureLB.

```plaintext
# kubectl apply -f https://gitlab.com/api/v4/projects/purelb%2Fpurelb/packages/generic/manifest/0.0.1/purelb-complete.yaml
```
Please note that due to Kubernetes' eventually-consistent architecture the first application of this manifest can fail. This happens because the manifest both defines a Custom Resource Definition and creates a resource using that definition. If this happens then apply the manifest again and it should succeed because Kubernetes will have processed the definition in the mean time.

### Install from Source
Installation from source isn't recommended for production systems but it's useful for development. The process is covered in the [PureLB gitlab repository](https://gitlab.com/purelb/purelb) readme.

### Verify Installation
PureLB should install a single instance of the allocator and an instance of lbnodeagent on each untainted node.

```plaintext
$ kubectl get pods --namespace=purelb --output=wide
NAME                        READY   STATUS    RESTARTS   AGE     IP               NODE        NOMINATED NODE   READINESS GATES
allocator-5cb95b946-5wmsz   1/1     Running   1          5h28m   10.129.3.152     purelb2-4   <none>           <none>
lbnodeagent-5689z           1/1     Running   2          5h28m   172.30.250.101   purelb2-3   <none>           <none>
lbnodeagent-86nlz           1/1     Running   3          5h27m   172.30.250.104   purelb2-1   <none>           <none>
lbnodeagent-f2cmb           1/1     Running   2          5h27m   172.30.250.103   purelb2-2   <none>           <none>
lbnodeagent-msrgs           1/1     Running   1          5h28m   172.30.250.105   purelb2-5   <none>           <none>
lbnodeagent-wrvrs           1/1     Running   1          5h27m   172.30.250.102   purelb2-4   <none>           <none>
```
