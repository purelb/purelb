---
title: "Multiple LoadBalancer Controllers"
description: "Describe Operation"
weight: 45
hide: [ "toc", "footer" ]
---
Starting with Kubernetes v1.22, multiple LoadBalancer controllers can be used in a single cluster. This allows each Service's LoadBalancer controller to be selected by adding [`spec.loadBalancerClass`](https://kubernetes.io/docs/concepts/services-networking/service/#load-balancer-class) to the Service definition.

### Configuring PureLB as Default LoadBalancer

By default, PureLB assumes it is the only Service LoadBalancer, and will therefore allocate addresses to LoadBalancers with no `spec.loadBalancerClass`.

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
To disable this, delete the DEFAULT_ANNOUNCER environment variable in the Allocator deployment yaml.

### Selecting a LoadBalancer
Once disabled as default, you need to specify PureLB as the desired LoadBalancer or else it will ignor the request. To select PureLB for an individual Service add loadBalancerClass to the service spec.

```yaml
apiVersion: v1
kind: Service
...
spec:
  loadBalancerClass: purelb.io/purelb
  ...
```

### What's the Use Case?
The primary purpose for this feature is to use PureLB in conjunction with Cloud Controllers. Cloud providers do not use independent Service LoadBalancers like PureLB. There is another type of controller called a Cloud Controller which provides multiple functions and is how cloud providers integrate Kubernetes with their environments. In addition to providing External Load Balancers, Cloud Controllers also integrate cloud storage, identity, and other capabilities.

A use case could be a Kubernetes cluster deployed in a cloud where there is also a private link to another network.  Routing to these private links could use BGP, therefore PureLB could be deployed with routing to advertise services into the network connected via the private link.

This feature is more about innovation, though: the ability to select LoadBalancers will enable any organization (cloud provider or independent) to add a choice of LoadBalancers for in-cloud Kubernetes clusters, enabling new functionality not present in current cloud LoadBalancers.
