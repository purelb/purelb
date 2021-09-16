---
title: "PureLB & IPv6"
description: "Describe Operation"
weight: 50
hide: toc, nextpage
---
IPv6 & IPv6 Dual Stack was recently enabled in k8s v1.21.  PureLB was developed while support for IPv6 was being undertaken in kubernetes, and during the development of PureLB IPv6 support was included.  The mechanism used to allocate addresses are similar between IPv4 and IPv6 in Linux therefore the logic was straight forward to implement.  This is good news as PureLB does support IPv6 and Dual Stack, however there are some caveats with the current release.  (We will remove these as we fix them)

### IPv6 Local Address Allocation fails.  
We have a bug for this, its probably a simple problem - should be fixed soon.  

### IPv6 Virtual Address work correctly.
IPv6 addresses can be allocated to _kube-lb0_ for distribution with routing protocols.  Its very likely that you will use this configuration.  The popular CNI's that support IPv6 do not support IPv6 in tunnels so a dual stack configuration is probably going to be a flat network.


### PureLB cannot create a Dual Stack Service.  
Currently PureLB cannot create a Dual Stack Service, a single service that has both an IPv4 and IPv6 address.  When dual stack was implemented additional service parameters were added

* ipFamilyPolicy: This enables the selection of Single or Dual stack
* ipFamilies: The list of IP families that are supported.

IP families is a common way to reference different type of IP networking.  In this case its used to select the mode of operation.

PureLB currently does not support the creation of a Service Group with multiple IP families, in fact it currently does not distinguish between them.  

_Support for Dual Stack Services is planned for the near future.  In the interim..._

The workaround is to create two Service Groups, one for IPv6 and another for IPv4, and then create a service for each address family. 

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv6-routed
  namespace: purelb
spec:
  local:
    aggregation: /128
    pool: 2001:470:8bf5:2:2::1-2001:470:8bf5:2:2::ffff
    subnet: 2001:470:8bf5:2::/64

---
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv4-routed
  namespace: purelb
spec:
  local:
    aggregation: /32
    pool: 172.30.200.155-172.30.200.160
    subnet: 172.30.200.0/24

---

apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/allocated-by: PureLB
    purelb.io/allocated-from: ipv4-routed
    purelb.io/service-group: ipv4-routed
  creationTimestamp: "2021-09-07T12:29:29Z"
  name: kuard-service
  namespace: adamd
  resourceVersion: "1471429"
  selfLink: /api/v1/namespaces/adamd/services/kuard-service
  uid: ca7c289e-bacb-45e0-9b3f-51b67eec7429
spec:
  allocateLoadBalancerNodePorts: true
  clusterIP: 10.152.183.252
  clusterIPs:
  - 10.152.183.252
  externalTrafficPolicy: Local
  healthCheckNodePort: 32083
  internalTrafficPolicy: Cluster
  ipFamilies:
  - IPv4
  ipFamilyPolicy: SingleStack
  ports:
  - nodePort: 31292
    port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: kuard
  sessionAffinity: None
  type: LoadBalancer
status:
  loadBalancer:
    ingress:
    - ip: 172.30.200.155

---

apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/allocated-by: PureLB
    purelb.io/allocated-from: ipv6-routed
    purelb.io/service-group: ipv6-routed
  creationTimestamp: "2021-09-07T14:50:55Z"
  name: kuard-service-ip6
  namespace: adamd
  resourceVersion: "1471462"
  selfLink: /api/v1/namespaces/adamd/services/kuard-service-ip6
  uid: 9d25ca30-3379-423a-92cb-7c66318be03e
spec:
  allocateLoadBalancerNodePorts: true
  clusterIP: fd98::ff04
  clusterIPs:
  - fd98::ff04
  externalTrafficPolicy: Local
  healthCheckNodePort: 30388
  internalTrafficPolicy: Cluster
  ipFamilies:
  - IPv6
  ipFamilyPolicy: SingleStack
  ports:
  - nodePort: 30091
    port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: kuard
  sessionAffinity: None
  type: LoadBalancer
status:
  loadBalancer:
    ingress:
    - ip: 2001:470:8bf5:2:2::1
```

