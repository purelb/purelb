---
title: "Install PureLB Bird router"
description: "Describe Operation"
weight: 20
hide: [ "toc", "footer" ]
draft: true
---

The PureLB repo includes a pre-packaged software router, [BIRD](https://bird.network.cz/).  This is suggested for deployments where load-balancer addresses are advertised by routing protocols but routing software is currently installed on the cluster.

The BIRD router is installed from the source repository.   The key installed component are:

1. Router Namespace.  A namespace is created for the router components

2. Configmap.  Bird is configured using a text file, the configmap projects that file into the Bird containers.

3. Bird Router Daemonset.  By default bird is deployed on all nodes.



## Create the configuration

The first step is to download the bird.yml and bird-cm.yml from the [gitlab repo](https://gitlab.com/purelb/bird_router)  Edit the configmap, bird-cm.yml to match your required network protocols and configuration.  The sample configmap differs from the sample provided by the bird source repo, modified for this use case.  It includes sample, commented out configurations for RIP, OSPF and BGP for both IPv4 and IPv6.

### Router configuration

When the container is created, Entrypoint updates the birdvars.conf file with the nodes ip address.  As shown below this is imported and the variable _k8sipaddr_ is used where the nodes ip address is needed.

```plaintext
# birdvars is created by docker_entrypoint.  It reads env variables for use in configuration
    include "/usr/local/include/birdvars.conf";

    # Router ID set using birdvars
    router id k8sipaddr;
```

PureLB adds IP addresses to kube-lb0 resulting in the routing table being updated as necessary by Linux.  These routes are imported into Bird using the _direct_ protocl and later referenced in the routing protocol configuration as *RTS_DEVICE*

```plaintext
# The direct protocol is used to import the routes added by PureLb to kube-lb0
    protocol direct {
      ipv4;
      ipv6;
      interface "kube-lb0";
    }
```

### Configuring Routing Protocols

#### BGP

The sample below sets the local autonomous system as 4200000003 and sets the neighbor as 172.30.250.1 as external.  This minimal configuration is ideal for us with routers where dynamic peer groups are used allowing k8s nodes to connect to the upstream BGP router without addition configuration. 

The local hosts ip address is substituted for the variable _k8sipaddr_.  

```plaintext
# BGP example, explicit name 'uplink1' is used instead of default 'bgp1'
    protocol bgp uplink1 {
      description "Example Peer";
      local k8sipaddr as 4200000003; # Autonomous
      neighbor 172.30.250.1 external; # Peer Neighbor
      #multihop 2;
      #hold time 90;            # Default is 240
      #password "secret";       # Password used for MD5 authentication

      ipv4 {                    # IPv4 unicast (1/1)
        # RTS_DEVICE matches routes added to kube-lb0 by protocol device
        export where source ~ [ RTS_STATIC, RTS_BGP, RTS_DEVICE ];
        import filter bgp_reject; # we are only advertizing
      };

      ipv6 {                    # IPv6 unicast
        # RTS_DEVICE matches routes added to kube-lb0 by protocol device
        export where  source ~ [ RTS_STATIC, RTS_BGP, RTS_DEVICE ];
        import filter bgp_reject;
      };
```
Do not change the variable k8ipaddr, the provides the per node ipaddress necessary for BGP operation. 


#### OSPF
This configuration enables OSPFv2 (IPv4) and OSPFv3 (IPv6).  There are many ways to configure OSPF however in this simple configuration the area is set to the root area (0.0.0.0).  The OSPF configuration requires that interface providing the routing protocol be configured, in the example below, any interface with that starts with "e*" will be used.  This is a sensible default as most Linux udev rules name ethernet interfaces starting with "e".  However, this will enable OSPF on every interface that starts with "e", if this is not desired the solution is to adjust the entrypoint script to identify which interface is desired and use a variable.


```plaintext
# OSPF example
    protocol ospf v2  {
     ipv4 {
       import none;
       export where source ~ [ RTS_STATIC, RTS_DEVICE ];
     };
     area 0 {
       interface "e*" {
         type broadcast;         # Detected by default
           cost 10;                # Interface metric
       };
     };
    }

    protocol ospf v3 {
      ipv6 {
        import none;
        export where source = RTS_STATIC, RTS_DEVICE;
      };
      area 0 {
        interface "e*" {
          type broadcast;		# Detected by default
          cost 10;		# Interface metric
        };
      };
    }
```

#### RIP
Sometimes the only thing supported is the old, simple RIP protocol  

```plaintext
    protocol rip {
      ipv4 {
      # Export direct, static routes and ones from RIP itself
        import none;
        export where source ~ [ RTS_DEVICE, RTS_STATIC, RTS_RIP ];
        };
        interface "e*" {
        update time 10;			# Default period is 30
       timeout time 60;		# Default timeout is 180
       #authentication cryptographic;	# No authentication by default
       #password "hello" { algorithm hmac sha256; }; # Default is MD5
       };
    }

    #protocol rip ng {
      ipv6 {
      # Export direct, static routes and ones from RIP itself
       import none;
       export where source ~ [ RTS_DEVICE, RTS_STATIC, RTS_RIP ];
       };
       interface "e*" {
         update time 10;                 # Default period is 30
         timeout time 60;                # Default timeout is 180
         #authentication cryptographic;   # No authentication by default
         #password "hello" { algorithm hmac sha256; }; # Default is MD5
       };
    }
```

## Installation


1. Create the namespace
```plaintext
$ kubectl create namespace router
```
2.  Apply the configmap
```plaintext
$ kubeclt apply -f bird-cm.yaml
```
3.  Create the Bird Daemonset
```plaintext
$ kubectl apply -f bird.yml
```

###
Check Operation
Bird containers should be running on all nodes that are not tainted
```plaintext
$ kubectl -n router get pods -o wide
NAME         READY   STATUS    RESTARTS   AGE   IP               NODE        NOMINATED NODE   READINESS GATES
bird-6rncj   1/1     Running   0          22h   172.30.250.101   purelb2-3   <none>           <none>
bird-bcd75   1/1     Running   0          22h   172.30.250.103   purelb2-2   <none>           <none>
bird-fcdnj   1/1     Running   0          22h   172.30.250.105   purelb2-5   <none>           <none>
bird-jj6l2   1/1     Running   0          22h   172.30.250.104   purelb2-1   <none>           <none>
bird-l7cff   1/1     Running   0          22h   172.30.250.102   purelb2-4   <none>           <none>
```



