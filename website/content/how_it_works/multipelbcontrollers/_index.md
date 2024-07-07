---
title: "Multiple LoadBalance Controllers"
description: "Describe Operation"
weight: 45
hide: [ "toc", "footer" ]
---
Beginning with k8s v1.22 multiple LoadBalancer Controllers can be used in a single cluster.  This allows either default LoadBalancer or other LoadBalancers to be selected by adding spec.loadBalancerClass to the the service definition.

PureLB supports loadBalancerClass and will ignore services that have a spec.loadBalancer class that is not _purelb.io/purelb_

The default installation of PureLB assumes it is the only Service LoadBalancer therefore services with no spec.loadBalancerClass will be allocated addresses by PureLB from the default Service Group.

### Configuring PureLB as Default LoadBalancer

The PureLB allocator listens for a Service Request, the configuration is part of the allocator deployment

```yaml
piVersion: apps/v1
kind: Deployment
spec:
  ...
    spec:
      containers:
      - env:
        - name: DEFAULT_ANNOUNCER
          value: PureLB
        ...
```
To disable PureLB as the default loadBalancer delete the DEFAULT_ANNOUNCER environment variable in the allocator deployment yaml

### Selecting a LoadBalancer
Once disabled as default, its necessary to specify PureLB as the desired LoadBalancer resulting in the default loadBalancer ignoring the request.

To select PureLB for an individual Service add loadBalancerClass to the service spec.

```yaml
apiVersion: v1
kind: Service
...
spec:
  loadBalancerClass: purelb.io/purelb
  ...
```

### Whats the Use Case
The primary purpose for this feature is use in conjunction with Cloud Controllers.  Cloud providers do not use independent Service LoadBalancers like PureLB.  There is another type of controller called a Cloud Controller, this controller provides multiple functions and is how Cloud providers integrate k8s with their environment.  In addition to providing External Load Balancers it also integrates cloud storage, identity and other capabilities.  The Cloud controller is always the default for LoadBalancer Services.  

A use case could be a k8s cluster deployed in a cloud where there is also a private link to another network.  Routing to these private links uses BGP, therefore PureLB could be deployed with routing to advertise services into the network connected via the Private link.

However, this feature is more about innovation, the ability to select between LoadBalancers will enable any organizations, Cloud Providers or independent to add a choice of LoadBalancers for Cloud k8s clusters enabling new functionality not present in the current generic Cloud LoadBalancers. 
