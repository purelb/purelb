---
title: "Installation"
description: "Install PureLB on your Kubernetes cluster using manifests or Helm."
weight: 20
---

PureLB can be installed using kubectl manifests or Helm. Both methods support optional BGP routing via the k8gobgp sidecar.

* [Prerequisites](prerequisites) -- Cluster requirements, ARP configuration, and port requirements.
* [Install with Manifests](manifest) -- Quick installation using kubectl apply.
* [Install with Helm](helm) -- Recommended for production, with full configuration control.
* [Your First LoadBalancer Service](first-service) -- End-to-end walkthrough from ServiceGroup creation to a working service.
