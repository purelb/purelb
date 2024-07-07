---
title: "Monitoring with kubectl"
description: "Describe Operation"
weight: 15
hide: [ "toc", "footer" ]
---

PureLB attempts to provide all of the information necessary to monitor and troubleshoot the health of Load Balancer services via kubectl without resorting to inspecting PureLB's pod logging.  PureLB's operational status and events are updated in the Load Balancer Services.  PureLB annotates services where it was the source of the allocated address.  If _allocated-by_ is not present, PureLB did not allocate the address. 

The simplest way to check the status of services if using the _kubectl describe_ command.

```plaintext
$ kubectl describe service kuard-svc-dual-remote 
Name:                     kuard-svc-dual-remote
Namespace:                adamd
Labels:                   <none>
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: remotedual
                          purelb.io/service-group: remotedual
Selector:                 app=kuard
Type:                     LoadBalancer
IP Family Policy:         RequireDualStack
IP Families:              IPv4,IPv6
IP:                       10.152.183.170
IPs:                      10.152.183.170,fd98::4078
LoadBalancer Ingress:     172.32.100.225, fc00:370:155:0:8000::
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
Endpoints:                10.1.217.204:8080,10.1.217.205:8080,10.1.238.137:8080
Session Affinity:         None
External Traffic Policy:  Local
HealthCheck NodePort:     30439
Events:
  Type    Reason              Age                From                Message
  ----    ------              ----               ----                -------
  Normal  AddressAssigned     11s                purelb-allocator    Assigned {Ingress:[{IP:172.32.100.225 Hostname: Ports:[]} {IP:fc00:370:155:0:8000:: Hostname: Ports:[]}]} from pool remotedual
  Normal  AnnouncingNonLocal  10s (x2 over 10s)  purelb-lbnodeagent  Announcing 172.32.100.225 from node mk8s3 interface kube-lb0
  Normal  AnnouncingNonLocal  10s (x2 over 10s)  purelb-lbnodeagent  Announcing 172.32.100.225 from node mk8s1 interface kube-lb0
  Normal  AnnouncingNonLocal  10s (x2 over 10s)  purelb-lbnodeagent  Announcing fc00:370:155:0:8000:: from node mk8s1 interface kube-lb0
  Normal  AnnouncingNonLocal  10s (x2 over 10s)  purelb-lbnodeagent  Announcing fc00:370:155:0:8000:: from node mk8s3 interface kube-lb0
```

The example above shows that PureLB allocated the address from the requested service group _virtualsg_, this information was added by the _allocator_.  The event messages are added by _lbnodeagent_ and show the nodes where the address was added.  As the address was added to multiple nodes, it is a virtual address as local addresses can only be added to a single node.

```plaintext
k describe service kuard-service 
Name:                     kuard-service
Namespace:                adamd
Labels:                   app=kuard
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: default
                          purelb.io/announcing-IPv4: mk8s2,enp1s0
Selector:                 app=kuard
Type:                     LoadBalancer
IP Family Policy:         SingleStack
IP Families:              IPv4
IP:                       10.152.183.155
IPs:                      10.152.183.155
LoadBalancer Ingress:     192.168.10.240
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  30310/TCP
Endpoints:                10.1.217.204:8080,10.1.217.205:8080,10.1.238.137:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason           Age                From                Message
  ----    ------           ----               ----                -------
  Normal  AddressAssigned  27m                purelb-allocator    Assigned {Ingress:[{IP:192.168.10.240 Hostname: Ports:[]}]} from pool default
  Normal  AnnouncingLocal  27m (x4 over 27m)  purelb-lbnodeagent  Node mk8s2 announcing 192.168.10.240 on interface enp1s0
```

This example shows that the default pool is part of the local address.  PureLB annotates these services with the node and interface where the address was announced as well us updating the event.  

This useful command command will show all nodes that are advertizing addresses locally.  The annotations make it easier find information in larger k8s clusters.


```plaintext
$ kubectl get services -A -o json | jq '.items[].metadata.annotations' | grep announcing
  "purelb.io/announcing-IPv4": "mk8s2,enp1s0",
  "purelb.io/announcing-IPv6": "mk8s2,enp1s0",
  "purelb.io/announcing-IPv4": "mk8s2,enp1s0"
```



