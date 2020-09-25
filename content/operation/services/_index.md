---
title: "Load Balancer Services"
description: "Describe Operation"
weight: 10
hide: toc, nextpage
---

PureLB uses the k8s services API therefore if a default service group has been defined, the instructions provided in the k8s documentation will result in a load balancer service being created.  This command will create a service type LoadBalancer resource for the deployment echoserver using using the default service group.

```plaintext
$ kubectl expose deployment echoserver --name=echoserver-service --port=80 --target-port=8080 --type=LoadBalancer

$ kubectl describe service echoserver
Name:                     echoserver
Namespace:                test
Labels:                   app=echoserver
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: default
                          purelb.io/announcing-interface: enp1s0
                          purelb.io/announcing-node: purelb2-1
Selector:                 app=echoserver
Type:                     LoadBalancer
IP:                       10.110.8.48
LoadBalancer Ingress:     172.30.250.53
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  32380/TCP
Endpoints:                <none>
Session Affinity:         None
External Traffic Policy:  Cluster
Events:                   <none>
```

### PureLB Annotations
PureLB uses annotations to configure functionality not native in the k8s API.  

Annotation | example | Description
-----------|---------|--------------
purelb.io/address-pool | purelb.io/address-pool: virtualsg |  Sets the Service Group that will be used to allocate the address (yes we know this is confusing, the annotation will change to sg in the near future)
purelb.io/allow-shared-ip | purelb.io/allow-shared-ip: sharingkey |  Allows the allocated address to be shared between multiple services in the same namespace


### k8s configuration options
The service API has options that impact how Loadbalancer services behave

Parameter | example | description
----|----|----
ExternalTrafficPolicy | ExternalTrafficPolicy: Cluster | Sets how purelb should add the service and kube-proxy forward traffic for the service
loadBalancerIP| loadBalancerIP: 172.30.250.80 | Allows the IP address to be statically defined in the service


### Creating a Service
The combination of the service group configuration, annotations and service configuration determine how the LoadBalancer is created.

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/address-pool: localaddr
  labels:
    app: echoserver
  name: servicetest
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
```
The sample above creates a service using a Service Group called localaddr.  PureLB will allocate from that service group and in these case the service group is configured with the local subnet therefore the following services will be created.

``` plaintext
$ kubectl describe service echoserver
Name:                     servicetest
Namespace:                servicetest
Labels:                   app=echoserver
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: localaddr
                          purelb.io/announcing-interface: enp1s0
                          purelb.io/announcing-node: purelb2-1
Selector:                 app=echoserver
Type:                     LoadBalancer
IP:                       10.110.8.48
LoadBalancer Ingress:     172.30.250.53
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  32380/TCP
Endpoints:                <none>
Session Affinity:         None
External Traffic Policy:  Cluster
Events:                   <none>
```
Describing the service displays the address provided by PureLB, in addition PureLB annotates the service to provide status information.  The annotations show that PureLB allocated the address from the localaddr Service Group.  Further, the annotations show that the address was added to a local interface, enp1s0 on k8s node purelb2-1.


```yaml
piVersion: v1
kind: Service
metadata:
  annotations:
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
Annotations:              purelb.io/allocated-by: PureLB
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
Events:                   <none>
```
Describing the service shows that the requested address has been allocated by Purelb from the pool virtualsub.  PureLB scanned the configured service groups to confirm the address is in a service group and not in use prior to allocation.  

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/address-pool: virtualsub
  labels:
    app: echoserver
  name: endpoints
  namespace: servicetest
spec:
  externalTrafficPolicy: Local
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
    subnet: '172.31.1.0/24'
    pool: '172.31.1.0/24'
    aggregation: '/32'

```

This sets _externalTrafficPolicy: Local_ changing the behavior of both PureLB and kube-proxy.  PureLB will only advertise the allocated address on nodes where the POD with the app label echoserver present.  KubeProxy will not configure forwarding to send traffic over the CNI to PODs.  


```plaintext
$ kubectl describe service endpoints
Name:                     endpoints
Namespace:                servicetest
Labels:                   app=echoserver
Annotations:              purelb.io/address-pool: virtualsub
                          purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: virtualsub
Selector:                 app=echoserver
Type:                     LoadBalancer
IP:                       10.108.97.71
LoadBalancer Ingress:     172.31.1.0
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  31391/TCP
Endpoints:                10.129.1.70:8080,10.129.3.146:8080,10.129.4.30:8080
Session Affinity:         None
External Traffic Policy:  Local
HealthCheck NodePort:     31400
Events:                   <none>
```
Describing the service shows that address was requested and allocated from the virtualsub pool, in this case the virtualsub pool sets the resulting address to 172.31.1.0/32.  This is the recommended configuration for External Traffic Policy: Local in this case the address is only added when the POD is present and therefore advertised via routing when the POD is present.  If the scale of the application changes, the number of nodes advertized will change.  

{{% alert theme="danger" %}} Aggregation.  Setting Service Group aggregation to a mask other than /32 (or /128) can result in traffic being send to nodes that do not have PODs, kubeproxy will not forward so the traffic will be lost.  There are use cases but caution should be exercised {{% /alert %}}

{{% alert theme="warning" %}} Local Addresses. Where the address is a local address External Traffic Policy is not supported.  PureLB will reset External Traffic Policy to Cluster.  Where an address is on the local network it can only be allocated to a single node, therefore this setting is not applicable {{% /alert %}}

{{% alert theme="warning" %}} Address Sharing.  External Traffic Policy: Local does not support Address Sharing.  Address sharing can result in nodes that do not have POD (endpoints) being advertised.  Kubeproxy will not forward so traffic would be lost.  PureLB does not allow this configuration {{% /alert %}}



