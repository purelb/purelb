---
title: "Install PureLB"
description: "Describe Operation"
weight: 10
hide: toc
---

PureLB can be installed from:


* Manifest
* Helm Chart
* Source repository
## Installed Components:


1. PureLB Namespace.  A namespace is created and annotated for all of the Purelb components
2. Custom Resource Definition.  PureLB uses two CRD's for configuration
3. Sample/Default configuration.  The default Purelb configuration and one sample Service Group configuration are added
4. Allocator deployment.  A deployment with a single instance of the allocator is installed on the cluster
5. lbnodeagent daemonset.  By default lbnodeagent is installed on all nodes.


### Preparing the Cluster
Preparing the cluster
Prior to the installation of PureLB, the k8s cluster should be installed with an operating Container Network Interface.  

#### Firewall Rules
Purelb uses a library called Memberlist to provide local network address failover faster than standard k8s timeouts would require.  If you plan to use local network address and have applied firewalls to your nodes, it is necessary to add a rule to allow the memberlist election to occur. The port used by Memberlist in PureLB is **Port 7934 UDP/TCP**, memberlist uses both TCP and UDP, open both.

{{% notice danger %}}
If UDP/TCP 7934 is not open and a local network address is allocated, Purelb will exhibit "split brain" behavior.  Each node will attempt to allocate the address where the local network addresses match and update v1/service.  This will cause the v1/service to continously update, the lbnodeagent logs show repeated attempts to register addresses and it it will appear that Purelb is unstable. 
{{% /notice %}}


#### ARP Behavior
{{% notice warning %}}
It is recommended that the ARP behavior changed from the Linux kernel default.  This is necessary if your using kubeproxy in IPVS model and is also good security practice.  By default Linux will answer ARP requests for addresses on any interface irrespective of the source, we recommend changing this setting so Linux only answers ARP requests for addresses on the interface it receives the request.  Linux sets this default to increase the the chance of successful communication. This change is made in sysconfig.
{{% /notice %}}


```plaintext
cat <<EOF | sudo tee /etc/sysctl.d/k8s_arp.conf
net.ipv4.conf.all.arp_filter=1
EOF
sudo sysctl --system

```
{{% notice danger %}}
PureLB will operate without making this change, however kubeproxy is set to IPVS mode and arp_filter is set to 0, all nodes will respond to locally allocated addresses as kubeproxy adds these addresses to kube-ipvs0
{{% /notice %}}

### Installing PureLB using Manifest

A Manifest is simply a concatenated set of yaml files that install all of the components of PureLB.

```plaintext
# kubectl apply -f https://gitlab.com/api/v4/projects/purelb%2Fpurelb/packages/generic/manifest/0.0.1/purelb-complete.yaml
```

### Install using the Helm Chart

```plaintext
$ helm repo add purelb https://gitlab.com/api/v4/projects/20400619/packages/helm/stable
$ helm repo update
$ helm install --create-namespace --namespace=purelb purelb purelb/purelb
```

### Install from Source.
Installation from source is covered in the [PureLB gitlab repository](https://gitlab.com/purelb/purelb) readme.





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
