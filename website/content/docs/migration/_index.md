---
title: "Migration"
description: "Upgrade an existing PureLB install: which migration guide applies to the version you run."
weight: 80
---

Pick the guide for the version you are upgrading **from**. If you are
installing PureLB for the first time, you need neither: follow the normal
install instructions.

You are on | API version | Guide
-----------|-------------|------
v0.16.x | `purelb.io/v2` | [v0.16.x to v0.17.0]({{< ref "/docs/migration/v0-17-0" >}})
v0.13.x, or any other pre-`v2` release (v0.15.x or earlier) | `purelb.io/v1` | [v0.13.x (purelb.io/v1) to v2]({{< ref "/docs/migration/v1-to-v2" >}}) — installs the current release directly

To see which API version you run:

```sh
kubectl get crd servicegroups.purelb.io -o jsonpath='{.spec.versions[*].name}'
```
