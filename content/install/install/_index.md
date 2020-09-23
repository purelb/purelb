---
title: "Install PureLB"
description: "Describe Operation"
weight: 10
hide: toc
---

PureLB can be installed from:


* Manifest
* Helm Chart (comming soon)
* Source repository






## Installation from Manifest

A Manifest is simply a conconated set of yaml files that install all of the components of PureLB.  The key installed commponents are:


1. PureLB Namespace.  A namespace is created and annotated for all of the Purelb components
2. Custom Resource Defination.  PureLB uses two CRD's for configuration
3. Sample/Default configuration.  The default Purelb configuration and one sample Service Group configuraiton are added
4. Allocator deployment.  A deployment with a single instance of the allocator is installed on the cluster
5. lbnodeagent daemonset.  By default lbnodeagent is installed on all nodes.


### Preparing the Cluster
Preparing the cluster
Prior to the installation of PureLB, the k8s cluster should be installed with an operating Container Network Interface.  

It is recommended that the ARP behavior changed from the Linux kernal default.  This is necessary if your using kubeproxy in IPVS model and is also good security practise.  By default Linux will answer ARP requests for addresses on any interface irrespective of the source, we recommend changing this setting so Linux only answers ARP requests for addresses on the interface it recieves the request.  Linux sets this default to increase the the chance of successful communication. This change is made in sysconfig.

```plaintext
cat <<EOF | sudo tee /etc/sysctl.d/k8s_arp.conf
net.ipv4.conf.all.arp_filter=1
EOF
sudo sysctl --system

```

_PureLB will operate without making this change, however kubeproxy is set to IPVS mode and arp_filter is set to 0, all nodes will respond to locally allocated addresses as kubeproxy adds these addresses to kube-ipvs0_

### Installing PureLB

```plaintext
# kubectl apply -f http://purelb.io/purelb/manifest
```

### Verify Installation
PureLB should install a single instance of the allocator and an instance of lbnodeagent on each untainted node.

```plaintext
$ kubectl get pods  -o wide
NAME                        READY   STATUS    RESTARTS   AGE     IP               NODE        NOMINATED NODE   READINESS GATES
allocator-5cb95b946-5wmsz   1/1     Running   1          5h28m   10.129.3.152     purelb2-4   <none>           <none>
lbnodeagent-5689z           1/1     Running   2          5h28m   172.30.250.101   purelb2-3   <none>           <none>
lbnodeagent-86nlz           1/1     Running   3          5h27m   172.30.250.104   purelb2-1   <none>           <none>
lbnodeagent-f2cmb           1/1     Running   2          5h27m   172.30.250.103   purelb2-2   <none>           <none>
lbnodeagent-msrgs           1/1     Running   1          5h28m   172.30.250.105   purelb2-5   <none>           <none>
lbnodeagent-wrvrs           1/1     Running   1          5h27m   172.30.250.102   purelb2-4   <none>           <none>
```
