# PureLB Deployment Descriptors

This directory holds a set of subdirectories that generate different deployment descriptors.
Each directory is a base where kustomize can be run.

* [default](default) - generates a manifest with the CRDs, RBAC, deployments and daemonsets but no configuration.
* [local](local) - generates a manifest with everything in [default](default) plus a simple local configuration.
* [epic](epic) - generates a manifest with everything in [default](default) plus a simple EPIC configuration.
