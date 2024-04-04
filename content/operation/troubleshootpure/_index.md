---
title: "Troubleshooting PureLB"
description: "Describe Operation"
weight: 40
hide: toc, nextpage
---

If PureLB isn't behaving the way you expect, it's easy to figure out what's going on.  PureLB uses the standard Linux networking libraries (iproute2/netlink) to configure addresses on nodes.

PureLB adds addresses to the interface that it identifies as the default interface (by looking for the default route), unless this election was overridden in the custom resource _lbnodeagent_ or the virtual interface _kube-lb0_.

Inspecting Linux interfaces and the Linux Routing table will show the load balancer addresses.  It is not necessary to log into the host to do this inspection.  Just like a CNI, the _lbnodeagent_ PODs are set to hostNetwork:true, so the container is isolated but uses the host network namespace.

```plaintext
$ kubectl get pods --namespace=purelb --output=wide
NAME                        READY   STATUS    RESTARTS   AGE    IP               NODE        NOMINATED NODE   READINESS GATES
allocator-5cb95b946-vmxqz   1/1     Running   0          106m   10.129.3.152     purelb2-4   <none>           <none>
lbnodeagent-5vx7m           1/1     Running   0          106m   172.30.250.102   purelb2-4   <none>           <none>
lbnodeagent-79sd4           1/1     Running   0          106m   172.30.250.101   purelb2-3   <none>           <none>
lbnodeagent-bpj95           1/1     Running   0          106m   172.30.250.105   purelb2-5   <none>           <none>
lbnodeagent-fkn89           1/1     Running   1          106m   172.30.250.103   purelb2-2   <none>           <none>
lbnodeagent-ssblg           1/1     Running   1          106m   172.30.250.104   purelb2-1   <none>           <none>
```

Prior to troubleshooting you must install any tools that you plan to use. In this case we'll install the _iproute_ package into _lbnodeagent-ssblg_ and run our commands there.
```plaintext
$ kubectl exec --namespace=purelb -it lbnodeagent-ssblg -- bash -l
[root@purelb2-1 /]# chmod +w /usr/*
[root@purelb2-1 /]# microdnf install -y iproute
```

Now we can use the _ip_ command to examine the routes:
```plaintext
[root@purelb2-1 /]# ip route show
default via 172.30.250.1 dev enp1s0  src 172.30.250.104  metric 100 
10.129.0.0/24 dev cni0 scope link  src 10.129.0.1 
10.129.1.0/24 via 172.30.250.103 dev enp1s0 
10.129.2.0/24 via 172.30.250.101 dev enp1s0 
10.129.3.0/24 via 172.30.250.102 dev enp1s0 
10.129.4.0/24 via 172.30.250.105 dev enp1s0 
172.17.0.0/16 dev docker0 scope link  src 172.17.0.1 
172.30.250.0/24 dev enp1s0 scope link  src 172.30.250.104 
172.30.250.1 dev enp1s0 scope link  src 172.30.250.104  metric 100 
172.31.0.0/24 dev kube-lb0 scope link  src 172.31.0.9 
172.31.2.0/24 dev kube-lb0 scope link  src 172.31.2.128 
```

On this node PureLB would identify enp1s0 as the local interface as it has the default route.  Inspect the interface:

```plaintext
[root@purelb2-1 /]# ip addr show dev enp1s0
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP qlen 1000
    link/ether 52:54:00:76:05:b6 brd ff:ff:ff:ff:ff:ff
    inet 172.30.250.104/24 brd 172.30.250.255 scope global enp1s0
       valid_lft forever preferred_lft forever
    inet 172.30.250.53/24 brd 172.30.250.255 scope global secondary enp1s0
       valid_lft forever preferred_lft forever
    inet 172.30.250.54/24 brd 172.30.250.255 scope global secondary enp1s0
       valid_lft forever preferred_lft forever
    inet6 2001:470:8bf5:1:1::2/128 scope global dynamic 
       valid_lft 3367sec preferred_lft 2367sec
    inet6 fe80::5054:ff:fe76:5b6/64 scope link 
       valid_lft forever preferred_lft forever
```
The addresses added by PureLB are displayed using a iproute2 commands, the additional addresses are displayed as secondary.

On the same node, addresses have been added to the virtual interface.  This caused routes to be added on the device kube-lb0 (above). To inspect kube-lb0:

```plaintext
[root@purelb2-1 /]# ip addr show dev kube-lb0
44: kube-lb0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN qlen 1000
    link/ether ee:f0:22:7d:70:0d brd ff:ff:ff:ff:ff:ff
    inet 172.31.1.225/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.2.128/24 brd 172.31.2.255 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.1.5/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.1.2/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.1.7/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.1.9/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet 172.31.0.9/24 brd 172.31.0.255 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet6 fe80::ecf0:22ff:fe7d:700d/64 scope link 
       valid_lft forever preferred_lft forever
```

Note that the addresses routes are correctly represented in the routing table.

For the addresses added by PureLB, the k8s service is authoritative so the Linux host should match k8s expected state.  If the linux network state does not match, there are misconfigurations that are possible.

An example is where multiple default routes have been added to the host.  This is not a valid configuration however Linux allows it to occur.  When there are two default routes, Linux picks the first default however this can cause unpredictable behavior.  PureLB does not operate in this manner, if this occurs, PureLB is unable to identify the local interface.   An error will be logged in the _lbnodeagent_ POD log and the address will be added to the virtual interface instead.

## Logging

The maintainers consider logging primarily a developer tool (everybody has an opinion), however, have tried to make the logs useful to all.  Logs are always a work in progress.

We don't believe that reading logs should be necessary on a day-to-day basis but if you're in a situation where logs can be helpful here's some info to help you use them:

PureLB pods run in the `purelb` namespace. There are two PureLB executables: the allocator and the lbnodeagent. The allocator typically runs a single pod per cluster and the lbnodeagent runs a pod on each node so you might need to examine several lbnodeagent logs before you find the one you're looking for. The `kubectl get pods --namespace=purelb --output=wide` command can be helpful as it shows you which lbnodeagent pod is running on which node.

We don't log timestamps since k8s adds them implicitly to all log messages. You can use `kubectl logs --timestamps` to see the k8s timestamps.
