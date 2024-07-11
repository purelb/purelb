---
title: "Netbox IPAM"
description: "Describe Operation"
weight: 50
hide: [ "toc", "footer" ]
---

PureLB can allocate IP addresses from [Netbox's](https://netbox.readthedocs.io/en/stable/) IP Address Management component. PureLB needs the Netbox instance's base URL and a Netbox Token, which are managed in Netbox's Admin console in the "Users" section. The Netbox user that owns the Token needs at least these Netbox permissions:

  * ipam.view_ipaddress
  * ipam.change_ipaddress

The Token is injected into PureLB's allocator pod as an environment variable that references a [Kubernetes Secret](https://kubernetes.io/docs/concepts/configuration/secret/). To install the token Secret, run:

```plaintext
$ kubectl create secret generic -n purelb netbox-client --from-literal=user-token="your-token"
```

The PureLB allocator can now be configured to request addresses from Netbox. An example:

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: netboxsg
  namespace: purelb
spec:
  netbox:
    url: http://your-netbox-host.your-domain.com/
```
