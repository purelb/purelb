---
title: "BIRD Router monitor & configure "
description: "Describe Operation"
weight: 30
hide: toc, nextpage
draft: true
---

## Monitoring
Each instance of the Daemonset is an individual router, however they configuration of each member is identical with the exception of the _RouterID_

The BIRD command line interface can be accessed inside each container.

```plaintext
$ kubectl get pods -o wide
NAME         READY   STATUS    RESTARTS   AGE   IP               NODE        NOMINATED NODE   READINESS GATES
bird-6rncj   1/1     Running   0          22h   172.30.250.101   purelb2-3   <none>           <none>
bird-bcd75   1/1     Running   0          22h   172.30.250.103   purelb2-2   <none>           <none>
bird-fcdnj   1/1     Running   0          22h   172.30.250.105   purelb2-5   <none>           <none>
bird-jj6l2   1/1     Running   0          22h   172.30.250.104   purelb2-1   <none>           <none>
bird-l7cff   1/1     Running   0          22h   172.30.250.102   purelb2-4   <none>           <none>

$ kubectl exec -it bird-jj6l2 -- /usr/local/sbin/birdc
BIRD 2.0.7 ready.

bird> show route 
Table master4:
0.0.0.0/0            unicast [kernel1 2020-09-26] (10)
	via 172.30.250.1 on enp1s0
10.129.1.0/24        unicast [kernel1 2020-09-26] (10)
	via 172.30.250.103 on enp1s0
10.129.2.0/24        unicast [kernel1 2020-09-26] (10)
	via 172.30.250.101 on enp1s0
10.129.3.0/24        unicast [kernel1 2020-09-26] (10)
	via 172.30.250.102 on enp1s0
10.129.4.0/24        unicast [kernel1 2020-09-26] (10)
	via 172.30.250.105 on enp1s0
172.31.1.225/32      unicast [direct1 2020-09-26] * (240)
	dev kube-lb0
172.31.1.9/32        unicast [direct1 2020-09-26] * (240)
	dev kube-lb0
172.31.1.2/32        unicast [direct1 2020-09-26] * (240)
	dev kube-lb0
172.31.1.5/32        unicast [direct1 2020-09-26] * (240)
	dev kube-lb0
172.30.250.1/32      unicast [kernel1 2020-09-26] (10)
	dev enp1s0
172.31.1.7/32        unicast [direct1 2020-09-26] * (240)
        dev kube-lb0
172.31.0.0/24        unicast [direct1 2020-09-26] * (240)
	dev kube-lb0
172.31.2.0/24        unicast [direct1 2020-09-26] * (240)
	dev kube-lb0

Table master6:
::/0                 unicast [kernel2 2020-09-26] (10)
	via fe80::5054:ff:fe9a:5b9 on enp1s0
bird> 
```

Therefore the basic status of the upstream router should should uniformly show each node as a neighbor as shown in the example below.

```plaintext
labrtr# show bgp summary

IPv4 Unicast Summary:
BGP router identifier 172.30.255.252, local AS number 4200000001 vrf-id 0
BGP table version 8806
RIB entries 32, using 5888 bytes of memory
Peers 7, using 143 KiB of memory
Peer groups 1, using 64 bytes of memory

Neighbor        V         AS MsgRcvd MsgSent   TblVer  InQ OutQ  Up/Down State/PfxRcd
*172.30.250.101 4 4200000003    1532    1339        0    0    0 22:10:56            7
*172.30.250.102 4 4200000003    1525    1339        0    0    0 22:10:57           10
*172.30.250.103 4 4200000003    1524    1339        0    0    0 22:10:58            9
*172.30.250.104 4 4200000003    1528    1339        0    0    0 22:10:54            7
*172.30.250.105 4 4200000003    1526    1340        0    0    0 22:11:00            9
172.30.255.1    4      65550   16169   18388        0    0    0 6d22h47m            7

Total number of neighbors 7
* - dynamic neighbor
5 dynamic neighbor(s), limit 100

labrtr# show ip ospf neighbor 

Neighbor ID     Pri State           Dead Time Address         Interface                        RXmtL RqstL DBsmL
172.30.250.101    1 Full/DROther      31.744s 172.30.250.101  enp6s0:172.30.250.1                  0     0     0
172.30.250.102    1 Full/DROther      30.931s 172.30.250.102  enp6s0:172.30.250.1                  0     0     0
172.30.250.103    1 Full/DROther      30.212s 172.30.250.103  enp6s0:172.30.250.1                  0     0     0
172.30.250.104    1 Full/DROther      34.250s 172.30.250.104  enp6s0:172.30.250.1                  0     0     0
172.30.250.105    1 Full/Backup       37.780s 172.30.250.105  enp6s0:172.30.250.1                  0     0     0
```
## Dynamic Configuration
The BIRD router can be dynamically reconfigured.  When triggered the BIRD will check the configuration syntax, and if it is correct apply the new configuration.  The daemonset container includes a script that watches the projected configmap file located in /usr/local/etc/bird.conf and when the configmap is updated reloads the configuration.  The configuration is merged with the current configuration limiting the disruption to routing distribution.  

{{% notice danger %}}
The configuration will also be reloaded by restarting/deleting the container however this will disrupt all routing protocols.  
{{% /notice %}}

Configuration events are reported in each container log

```plaintext
kubectl logs bird-jj6l2 
bird -fR
Setting up watches.
Watches established.
2020-09-26 19:47:30.447 <INFO> Graceful restart started
2020-09-26 19:47:30.447 <INFO> Started
2020-09-26 19:47:35.883 <INFO> Graceful restart done
2020-09-27 18:06:30.999 <INFO> Reconfiguring
2020-09-27 18:06:30.999 <INFO> Reconfigured
```