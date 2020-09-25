---
title: "IPAM"
description: "Describe Operation"
weight: 26
hide: toc, nextpage
---

IP Address Management (IPAM) is a critical function in any network, ensuring that addresses are allocated in to devices in a manner that results in the desired connectivity requires planning and ongoing management.  PureLB includes an integrated IPAM/address allocator, however can also support external IPAM systems.

This integrated allocator is installed with PureLB and is always present, allowing addresses to be allocated by the cluster for some use cases and in other cases from the external IPAM.  All addresses are allocated from Service Groups, in the case of the local allocator, those service groups contain the IP Address information for the addresses that will be allocated.  In the case of external IPAM, the Service Group contains the configuration information necessary for the external IPAM system

By moving IPAM out of the k8s cluster, PureLB can reduce the number of places where IP addresses are allocated, reducing the possibility of configuration errors and simplifying system management.

External IPAM is common in large scale systems, the combination of static addresses, dynamic address and routing is to complex to keep in a spreadsheet.  Add that public IPv4 addresses are increasingly scarce and many large scale systems re-use private address space behind network address translators, a complex management picture begins to unfold. 

The current version of PureLB supports the popular open source IPAM system, [Netbox](https://github.com/netbox-community/netbox)  Instead of simply using Netbox to statically track addresses used by PureLB, the allocator can request addresses for Service Groups directly from Netbox. 

The IPAM system's configuration is defined Service Groups, when the address is returned from the external IPAM it is applied to interfaces in the same manner as an address from the local IPAM

It is straightforward to add external IPAM systems to Netbox, we encourage others to add support for other IPAM systems, and we will be adding additional IPAM functionality in the future.