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
