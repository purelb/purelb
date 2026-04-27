---
title: "Contributing"
description: "Build PureLB from source, contribute code, and get help."
weight: 95
---

## Source Code

PureLB is developed on GitHub: [github.com/purelb/purelb](https://github.com/purelb/purelb)

## Building from Source

PureLB is built with `make` using [ko](https://ko.build/) for container images. See [BUILDING.md](https://github.com/purelb/purelb/blob/main/BUILDING.md) for detailed instructions.

Key targets:

```sh
make check          # Run go vet and go test -race -short
make generate       # Generate Kubernetes client code
make crd            # Generate CRD manifests
make image          # Build container images via ko
make manifest       # Generate deployment YAML via kustomize
make helm           # Package Helm chart
make scan           # Run govulncheck
make plugin         # Build kubectl-purelb binary
```

## Getting Help

- **Slack:** [#purelb-users](https://kubernetes.slack.com/messages/purelb-users) in the Kubernetes workspace
- **Issues:** [GitHub Issues](https://github.com/purelb/purelb/issues)

## Credits

PureLB was forked from MetalLB in 2020. We believed a better solution was to use Linux networking functionality instead of working around it. We would like to acknowledge the original developer, [Dave Anderson](https://www.dave.tf/).
