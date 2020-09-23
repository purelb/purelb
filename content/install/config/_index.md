---
title: "Initial Configuration"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---


The configuration of PureLB is simple and is undertaken using Custom Resources.  A single system configuration that enables default overides and as many Service Groups needed to express external IP address ranges.

```plaintext
$ kubectl api-resources --api-group=purelb.io
NAME            SHORTNAMES   APIGROUP    NAMESPACED   KIND
lbnodeagents    lbna         purelb.io   true         LBNodeAgent
servicegroups   sg           purelb.io   true         ServiceGroup
```

### lbnodeagent
The installation procedure will have created lbnodeagent.purelb./io/default  In most cases these defaults are suitable

```plaintext
$ kubectl describe lbnodeagent.purelb.io/default 
Name:         default
Namespace:    purelb
Kind:         LBNodeAgent
Spec:
  Local:
    Extlbint:  kube-lb0
    Localint:  default
```
* Extlbint:  This sets the name of the virtual interface used for virtual addresses.  The default is kube-lb0.  (If you change it, and are using the purelb bird configuration, make sure you update the bird.cm)
* Localint: Purelb automatically identifies the interface that is connected to the local network and the address range used.  The default setting enables this automatic functionality.  If you wish to override this functionility and specify which interface Purelb should add local address, specify it here. Currently it must be uniform throughout the cluster.  (note that you need to make sure that this interface has appropriate routing, the algorithmic selector finds the interface with the default route, the interface that is most likely to have global communications.)


### Service Groups