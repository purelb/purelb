---
title: "Monitoring with kubectl"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---

PureLB attempts to provide all of the information necessary to monitor and troubleshoot the health of Load Balancer services via kubectl without resorting to inspecting PureLB's POD logging.  PureLB's operational status and events are updated in the Load Balancer Services.  PureLB annotates services where it was the source of the allocated address.  If _allocated-by_ is not present, PureLB did not allocate the address. 

The simplest way to check the status of services if using the _kubectl describe_ command.

```plaintext
$kubectl describe service specific-address2
Name:                     specific-address2
Namespace:                adamd
Labels:                   app=echoserver3
Annotations:              purelb.io/service-group: virtualsg
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: virtualsub
Selector:                 app=echoserver3
Type:                     LoadBalancer
IP:                       10.104.193.121
IP:                       172.31.1.225
LoadBalancer Ingress:     172.31.1.225
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  31377/TCP
Endpoints:                10.129.3.151:8080,10.129.4.33:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason              Age                From                Message
  ----    ------              ----               ----                -------
  Normal  AnnouncingNonLocal  52s (x2 over 22h)  purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-4 interface kube-lb0
  Normal  AnnouncingNonLocal  37s (x5 over 43s)  purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-5 interface kube-lb0
  Normal  AnnouncingNonLocal  37s (x4 over 41s)  purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-3 interface kube-lb0
  Normal  AnnouncingNonLocal  37s                purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-2 interface kube-lb0
  Normal  AnnouncingNonLocal  37s                purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-4 interface kube-lb0
  Normal  AnnouncingNonLocal  36s                purelb-lbnodeagent  Announcing 172.31.1.225 from node purelb2-1 interface kube-lb0

``` 

The example above shows that PureLB allocated the address from the requested service group _virtualsg_, this information was added by the _allocator_.  The event messages are added by _lbnodeagent_ and show the nodes where the address was added.  As the address was added to multiple nodes, it is a virtual address as local addresses can only be added to a single node.

```plaintext
$ kubectl describe service echoserver21 
Name:                     echoserver21
Namespace:                toby
Labels:                   app=echoserver1
Annotations:              purelb.io/service-group: default
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: default
                          purelb.io/announcing-interface: enp1s0
                          purelb.io/announcing-node: purelb2-1
Selector:                 app=echoserver1
Type:                     LoadBalancer
IP:                       10.110.8.48
LoadBalancer Ingress:     172.30.250.53
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  32380/TCP
Endpoints:                <none>
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason           Age                From                Message
  ----    ------           ----               ----                -------
  Normal  AnnouncingLocal  19m                purelb-lbnodeagent  Node purelb2-4 announcing 172.30.250.53 on interface enp1s0
```

This example shows that the default pool is part of the local address.  PureLB annotates these services with the node and interface where the address was announced as well us updating the event.  

This useful command command will show all nodes that are advertizing addresses locally.  The annotations make it easier find information in larger k8s clusters.


```plaintext
 $ kubectl get services -A -o json | jq ' .items[].metadata.annotations' | grep announcing-node
  "purelb.io/announcing-node": "purelb2-1"
  "purelb.io/announcing-node": "purelb2-1"
```



