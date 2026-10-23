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

## Load-balancer-visible changes

- **A FlowSchema in every install method.** `purelb-lbnodeagent-leases` puts the
  node agents' Lease `get`/`create`/`update`/`delete` in the built-in
  `leader-election` priority level (before: `workload-low` via the
  `service-accounts` schema), so a busy API server can't delay renewals behind
  other workloads. Verified against a real API server: the agent service
  account's Lease requests dispatch under this schema at `leader-election`.
  Uses `flowcontrol.apiserver.k8s.io/v1` (Kubernetes 1.29+; the 1.31 minimum
  covers it).

- **Helm: the lbnodeagent container's root filesystem is read-only**, as it
  has long been in the kustomize manifests (`lbnodeagent.containerSecurityContext.readOnlyRootFilesystem`
  was `false` in the chart only). A values override that set it explicitly is
  unaffected.

- **CRDs carry their release, and a stale CRD is reported.** Packaging stamps
  every PureLB CRD with `purelb.io/crd-version: <release>`.
  - The allocator and agents compare it with their own release at startup and
    every 10 minutes: `purelb_crd_stale{crd}`, an info log and a Warning event
    when a CRD turns stale. They keep running. New RBAC: `get` on
    `customresourcedefinitions`, limited by `resourceNames` to PureLB's seven.
  - Helm: `helm install`/`upgrade` fails before changing anything when an
    installed PureLB CRD is from another release (Helm never upgrades
    `crds/`). `skipCRDVersionCheck=true` skips it.
  - Helm users always apply `install-crds-<ver>.yaml` (the chart installs every
    CRD); the Helm docs no longer point `gobgp.enabled=false` users at the
    nobgp CRDs, which would leave the egress CRDs stale.
  - CRDs applied by an earlier release have no stamp, so the first upgrade to
    this release reports them stale until its CRDs are applied, which the
    upgrade procedure already requires.

- **Release images are signed and carry build provenance.** The release job
  signs the allocator and lbnodeagent images keyless with cosign (GitHub OIDC,
  recorded in Rekor) and pushes GitHub build-provenance attestations. Verify:
  `cosign verify --certificate-identity-regexp 'https://github.com/purelb/purelb/'
  --certificate-oidc-issuer https://token.actions.githubusercontent.com <image>`
  or `gh attestation verify oci://<image> -R purelb/purelb`. **First run is the
  first release tag after this merges** (the job runs only on tags).
- **Base images pinned by digest** (`.ko.yaml`): distroless static `nonroot`
  and `latest` multi-arch indexes; the comment there gives the refresh command.

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
