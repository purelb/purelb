---
title: "Creating LoadBalancers"
description: "Describe Operation"
weight: 10
hide: [ "toc", "footer" ]
---

ServiceGroup configuration, annotations, and Service configuration determine how a LoadBalancer is created. Here's an example Service using the `localaddr` ServiceGroup.
```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/service-group: localaddr
  labels:
    app: echoserver
  name: servicetest
  namespace: servicetest
spec:
  externalTrafficPolicy: Cluster
  ipFamilies:
  - IPv4
  ipFamilyPolicy: SingleStack
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: echoserver
  sessionAffinity: None
  type: LoadBalancer
```
PureLB will allocate an address from `localaddr`, and assuming that `localaddr` has been configured with the local subnet, the following Service will be created:

``` plaintext
$ kubectl describe service echoserver
Name:                     servicetest
Namespace:                servicetest
Labels:                   app=echoserver
Annotations:              purelb.io/service-group: localaddr
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: localaddr
                          purelb.io/announcing-IPv4: pubelb2-1, enp1s0
Selector:                 app=echoserver
Type:                     LoadBalancer
IP Family Policy:         SingleStack
IP Families:              IPv4
IP:                       10.110.8.48
LoadBalancer Ingress:     172.30.250.53
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  32380/TCP
Endpoints:                10.1.217.204:8080,10.1.217.205:8080,10.1.238.137:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:Type    Reason           Age                From                Message
  ----    ------           ----               ----                -------
  Normal  IPAllocated      92m (x2 over 92m)  purelb-allocator    Assigned IP 172.30.250.53 from pool localaddr
  Normal  AnnouncingLocal  92m (x2 over 92m)  purelb-lbnodeagent  Node node3 announcing 172.30.250.53 on interface enp1s0
```
Describing the service displays the address provided by PureLB, in addition PureLB annotates the service to provide status information.  The annotations show that PureLB allocated the address from the localaddr ServiceGroup.  Further, the annotations show that the address was added to a local interface, enp1s0 on k8s node purelb2-1.


```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/service-group: virtualsub
  labels:
    app: echoserver
  name: specificaddress
  namespace: servicetest
spec:
  externalTrafficPolicy: Cluster
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: echoserver
  sessionAffinity: None
  type: LoadBalancer
  loadBalancerIP: 172.31.1.225
```
This example shows the use of an address allocated in the service. 

```plaintext
$ kubectl describe service -n adamd specificaddress2
Name:                     specificaddress
Namespace:                adamd
Labels:                   app=echoserver
Annotations:              purelb.io/service-group: virtualsub
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: virtualsub
Selector:                 app=echoserver3
Type:                     LoadBalancer
IP Family Policy:         SingleStack
IP Families:              IPv4
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
  Normal  IPAllocated         47m (x2 over 47m)  purelb-allocator    Assigned IP 172.31.1.225 from pool virtualsub
  Normal  AnnouncingNonLocal  47m                purelb-lbnodeagent  Announcing 172.31.1.225 from node node3 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.225 from node node1 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.225 from node node2 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.225 from node node5 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.225 from node node4 interface kube-lb0
```
Describing the service shows that the requested address has been allocated by PureLB from the `virtualsub` ServiceGroup. PureLB scanned the configured ServiceGroups to confirm the address is in a ServiceGroup and not in use prior to allocation.

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/service-group: virtualsub
  labels:
    app: echoserver
  name: endpoints
  namespace: servicetest
spec:
  externalTrafficPolicy: Local
  ipFamilies:
  - IPv4
  ipFamilyPolicy: SingleStack
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: echoserver
  sessionAffinity: None
  type: LoadBalancer
---
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: virtualsub
spec:
  local:
    v4pools:
    - subnet: '172.31.1.0/24'
      pool: '172.31.1.0/24'
      aggregation: '/32'
```

This sets `externalTrafficPolicy: Local`, changing the behavior of both PureLB and `kube-proxy`. PureLB will only advertise the allocated address on nodes that host pods with the app label `echoserver`. `kube-proxy` will not configure forwarding to send traffic over the CNI to pods.

```plaintext
$ kubectl describe service endpoints
Name:                     endpoints
Namespace:                servicetest
Labels:                   app=echoserver
Annotations:              purelb.io/service-group: virtualsub
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: virtualsub
Selector:                 app=echoserver
Type:                     LoadBalancer
IP Family Policy:         SingleStack
IP Families:              IPv4
IP:                       10.108.97.71
LoadBalancer Ingress:     172.31.1.0
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  31391/TCP
Endpoints:                10.129.1.70:8080,10.129.3.146:8080,10.129.4.30:8080
Session Affinity:         None
External Traffic Policy:  Local
HealthCheck NodePort:     31400
Events:
  Type    Reason              Age                From                Message
  ----    ------              ----               ----                -------
  Normal  IPAllocated         47m (x2 over 47m)  purelb-allocator    Assigned IP 172.31.1.0 from pool virtualsub
  Normal  AnnouncingNonLocal  47m                purelb-lbnodeagent  Announcing 172.31.1.0 from node node3 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.0 from node node1 interface kube-lb0
  Normal  AnnouncingNonLocal  25m                purelb-lbnodeagent  Announcing 172.31.1.0 from node node2 interface kube-lb0
```
Describing the service shows that the address was requested and allocated from the `virtualsub` pool, which sets the resulting address to 172.31.1.0/32.  This is the recommended configuration for `externalTrafficPolicy: Local` as the address is only added to a node's `kube-lb0` when a pod is running on that node, and therefore advertised via routing when the pod is present. If the scale of the application changes, the number of nodes advertised will change.

{{% notice danger %}} Aggregation.  Setting ServiceGroup aggregation to a mask other than /32 (or /128) can result in traffic being sent to nodes that do not have pods. `kube-proxy` will not forward so the traffic will be lost.  There are valid use cases but be careful! {{% /notice %}}

{{% notice warning %}} Local Addresses. Local addresses can only be added to a single node, therefore `externalTrafficPolicy: Local` not applicable. PureLB supports only `externalTrafficPolicy: Cluster` for local addresses and will reset External Traffic Policy to Cluster. {{% /notice %}}

{{% notice warning %}} Address Sharing. `externalTrafficPolicy: Local` does not support Address Sharing. Address sharing can result in nodes that do not have pods (endpoints) being advertised.  `kube-proxy` will not forward, so traffic would be lost, so PureLB does not allow this configuration. {{% /notice %}}

### Kubernetes Configuration Options
The service API has options that impact how LoadBalancer Services behave.

Parameter | example | description
----|----|----
ExternalTrafficPolicy | `externalTrafficPolicy: Cluster` | Sets how PureLB should add the service and kube-proxy forward traffic for the service
allocateLoadBalancerNodePorts | `allocateLoadBalancerNodePorts: false` |  By default nodeports are added for loadbalancers but are seldom required
loadBalancerClass | `loadBalancerClass: purelb.io/purelb` | When multiple loadbalancer controllers are present, select the specified controller
ipFamilyPolicy | `ipFamilyPolicy: PreferDualStack` | Selects which address families should be added to the services, SingleStack, PreferDualStack, RequireDualStack
ipFamilies | `ipFamilies: IPv6` | Selects IPv4 and/or IPv6

### PureLB Annotations
PureLB uses annotations to configure functionality not native in the k8s API.

Annotation | example | Description
-----------|---------|--------------
purelb.io/service-group | `purelb.io/service-group: virtualsg` |  Sets the ServiceGroup that will be used to allocate the address
purelb.io/allow-shared-ip | `purelb.io/allow-shared-ip: sharingkey` |  Allows the allocated address to be shared between multiple services as long as they expose different ports
purelb.io/addresses | `purelb.io/addresses: 172.30.250.80,ffff::27` | Assigns the provided addresses instead of allocating addresses from the ServiceGroup address pool
