# Egress — release material (raw notes)

Scratch list of what the egress work (`feat/egress`) changes, for the release
notes and docs of the release that ships it. Not user-facing prose.

## Requirements

- **Kubernetes 1.31 or newer, for every install.** The CRDs validate addresses
  and networks with the CEL IP and CIDR functions (`isIP`, `isCIDR`, `ip()`).
  The API server rejects them before 1.31: measured on 1.29 and 1.30, which
  refuse the CRDs, and 1.31 to 1.33, which accept them. This includes the core
  LBNodeAgent CRD (`spec.egress.excludedNetworks`, `podNetworks`), so it
  applies to load-balancer-only installs too. Stated in the prerequisites page
  and the chart's `kubeVersion`.

## New API

- Kinds: `Egress`, `ClusterEgress` (cluster-scoped), `EgressEndpoints`,
  `EgressMembers`, `EgressNodeState` (cluster-scoped).
- `ServiceGroup.spec.egress` and `LBNodeAgent.spec.egress`. Absent, egress is
  refused / disabled.

## Install methods

Egress ships like k8gobgp: a kustomize component (`deployments/components/egress`)
and a Helm value, `egress.enabled` (default true). It is idle until an
LBNodeAgent sets `spec.egress.enabled`.

| Install | Manifest | CRDs only |
| --- | --- | --- |
| Everything: PureLB + k8gobgp + egress (default) | `install-<ver>.yaml` | `install-crds-<ver>.yaml` |
| PureLB + k8gobgp | `install-noegress-<ver>.yaml` (new) | `install-crds-noegress-<ver>.yaml` (new) |
| PureLB only | `install-nobgp-<ver>.yaml` | `install-crds-nobgp-<ver>.yaml` |

- **`install-<ver>.yaml` now includes egress.** Users who want today's
  PureLB + k8gobgp install without egress switch to `install-noegress-<ver>.yaml`.
- **`install-nobgp-<ver>.yaml` now also means "without egress".**
- Helm always installs the egress CRDs (Helm can't template `crds/`);
  `egress.enabled=false` leaves out the egress RBAC.
