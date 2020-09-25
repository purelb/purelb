---
title: "External IPAM"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---

PureLB supports external IPAM systems.  These are configured and access via Service Groups, and are therefore transparent to developers.  The installation and configuration of the specific IPAM systems is beyond the scope of PureLB's documentation, how PureLB is configured is documented.


{{%expand "Netbox" %}}
PureLB can allocate IP addresses from [Netbox's](https://netbox.readthedocs.io/en/stable/) IP Address Management component. While Netbox offers many different configuration options, PureLB has chosen a specific approach to requesting addresses from Netbox.

PureLB needs the Netbox instance's base URL and a Netbox Token, which
are managed in Netbox's Admin console in the "Users" section. The
Netbox user that owns the Token needs at least these permissions:

  * ipam.view_ipaddress
  * ipam.change_ipaddress

The Token is injected into PureLB's allocator pod as an environment
variable that references a k8s secret. To install the Token secret
into your k8s cluster, run:

```plaintext
$ kubectl create secret generic -n purelb netbox-client --from-literal=user-token="your-token"
```

The PureLB allocator must now be configured to request addresses from Netbox. An example:

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: acnodal
  namespace: purelb
spec:
  egw:
    url: http://your-netbox-host.com/
```

{{% /expand%}}
