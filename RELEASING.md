# Releasing PureLB

This document is the canonical release procedure for PureLB. The primary
defense against drift is the CI gate stack (`make check-deps` for k8gobgp
CRDs; `make check-helm-rbac-source` for Helm vs kustomize RBAC); this
checklist is the secondary defense, covering what the gates cannot
enforce (procedural drift, missed doc bumps, post-tag rollback).

Run through every section in order. Each item exists because a prior
release missed it.

## Pre-flight (hard gates)

- [ ] `git status` clean on `main`, `git pull` up to date.
- [ ] `make check` passes locally. **This includes `make check-deps`
      (CRD pin consistency) and `make check-helm-rbac-source` (Helm RBAC
      template is a thin wrapper around `deployments/components/gobgp/gobgp-rbac.yaml`).
      If `check-deps` fails, you have a CRD/version mismatch and must NOT
      proceed — fix it via the [Dependency review](#dependency-review-k8gobgp)
      below. If `check-helm-rbac-source` fails, the Helm template lost its
      injection marker or someone added inline rules — restore the marker,
      move any rule additions to the kustomize source.**
- [ ] **Dependabot triage**: `gh pr list -R purelb/purelb -l dependencies`.
      For each open PR: merge, close with reason, or explicitly defer to a
      named target release. **Empty queue or every PR explicitly
      dispositioned — not "skip for now."**
- [ ] Decide release type: patch (vX.Y.Z+1) for dep bumps and bug fixes,
      minor (vX.Y+1.0) for new features. Set `NEW=vX.Y.Z`, `OLD=vX.Y.Z-1`.

## Dependency review (k8gobgp)

PureLB bundles k8gobgp as a sidecar; its version is pinned in 4 places
plus the regenerated CRD files. The `check-deps` gate enforces that the
CRDs match the pin, but won't tell you when upstream has shipped a newer
release worth picking up.

- [ ] `gh release list -R purelb/k8gobgp --limit 5` — note the latest tag.
- [ ] For every release between the old pin and the new one:
      `gh release view vX.Y.Z -R purelb/k8gobgp`. Look for: new CRDs,
      breaking config changes, sidecar contract changes (env vars, sockets,
      ports). Note material findings in the PR description and in the
      GitHub release notes (`generate_release_notes: true` won't surface
      these — humans must).
- [ ] Update IN ORDER:
      1. `Makefile`: `GOBGP_TAG ?= vX.Y.Z` and `GOBGP_IMAGE_TAG ?= X.Y.Z`
         (note the `v` prefix difference — the GitHub release URL uses `v`,
         the docker image tag does not, because `docker/metadata-action`
         strips the prefix when publishing)
      2. `build/helm/purelb/values.yaml`: `tag: "X.Y.Z"` under `gobgp.image`
      3. `deployments/components/gobgp/gobgp-patch.yaml`: `image:` line
      4. `website/content/docs/reference/helm-values/_index.md`: documented
         tag value in the gobgp section
- [ ] `make fetch-gobgp-crd`. The target writes one file per CRD and
      validates that all upstream CRDs were extracted. **If it fails with
      "k8gobgp likely added a new CRD"**, add a per-name `kustomize cfg
      grep 'metadata.name=NEW_CRD\.bgp\.purelb\.io'` line to the target
      (note the escaped dots — kustomize cfg grep treats unescaped dots as
      path separators), then re-run.
- [ ] Commit any non-empty diff to `deployments/components/gobgp/gobgp-*-crd.yaml`.
- [ ] `make check-deps` PASSES. If it doesn't pass locally, neither will
      CI on the PR.
- [ ] Final sweep: `grep -rn "k8gobgp:0\." Makefile build/ deployments/components/ website/content/`
      — every hit should be the new tag. Misses here are the #1 source of
      past drift.

## Version-string bump (PureLB itself)

- [ ] `grep -rn "$OLD" README.md BUILDING.md website/content/docs/`
- [ ] Update each hit to `$NEW`. Watch for:
      - `README.md`: install URLs (5 occurrences as of v0.16.3) and the
        helm OCI install command
      - `BUILDING.md`: example tag in the CI/CD section (cosmetic; bump anyway)
      - `website/content/docs/installation/manifest/_index.md`
      - `website/content/docs/installation/helm/_index.md`
      - `website/content/docs/migration/_index.md` (NOTE: leave `v0.15.x`
        and other "migrating FROM" historical references alone — only bump
        current-version strings)
- [ ] Re-run the grep; expected hit count = 0.

## Do NOT touch

These look like version references but aren't:

- `build/helm/purelb/Chart.yaml` — `version: 0.0.0` and `appVersion: 0.0.0`
  are intentional placeholders; `make helm` overrides them via `--version`
  and `--app-version` from `$SUFFIX` at package time.
- `build/helm/purelb/values.yaml` lines 8 and 11 — `DEFAULT_REPO` /
  `DEFAULT_TAG` are sed-substituted at package time.
- `deployments/purelb-*.yaml`, `deployments/install-*.yaml`,
  `deployments/crds/purelb.io_*.yaml`, `pkg/generated/`, `build/build/` —
  gitignored or generated; any local copies are stale build artifacts.
  (Two stuck-tracked snapshots `deployments/purelb-v0.16.0.yaml` and
  `purelb-nobgp-v0.16.0.yaml` exist for legacy reasons; ignore them.)
- `cmd/kubectl-purelb/main.go` (`version = "dev"`) and
  `internal/logging/logging.go` (`release` var) — both set by ldflags at
  build time. Default literals are dev-mode fallbacks.
- `docs/*.md` — historical planning documents; their version references
  describe the state at the time of writing.

## Pre-tag local verification

Before pushing the branch, run a clean rebuild to surface anything CI
would catch — but with a 6-minute round-trip saved per failure:

```bash
make check                              # vet + race tests + check-deps + check-helm-rbac-source
SUFFIX=$NEW make manifest               # render the install manifest
SUFFIX=$NEW make install-manifest       # render the standalone install
SUFFIX=$NEW make helm                   # package the chart
grep "k8gobgp:" deployments/install-${NEW}.yaml          # confirms new tag in output
grep bgpnodestatuses build/build/purelb/templates/gobgp-rbac.yaml  # confirms rule injection
```

**Required (not "if handy"): clean-cluster smoke test.** Per project
convention, pre-existing CRDs/state mask real ordering bugs. Until CI has
an automated end-to-end smoke test, this manual run is the only thing
standing between a known bug and your users.

**You must run both pre-tag smoke tests.** The manifest path and the
Helm chart path are distinct artifacts; v0.16.1–v0.16.4 shipped a broken
Helm chart for 32+ days because only the manifest path was smoke-tested.

Pre-tag, there is no "OCI vs classic repo" distinction yet — neither
distribution channel has a chart for `$NEW` until the tag is pushed and
CI publishes. Pre-tag smoke test B installs the local `.tgz` that
`make helm` just produced; that is the exact byte stream both
distribution channels will serve post-release. The OCI-vs-classic
distinction is validated **post-release** in [Post-release
verification](#post-release-verification).

### Smoke test A — manifest install

```bash
# On a clean kind/k3d cluster (kubectl context set to it):
kubectl apply -f deployments/install-crds-${NEW}.yaml
kubectl apply -f deployments/install-${NEW}.yaml
kubectl -n purelb-system get pods                          # all Running
kubectl -n purelb-system get pod -l name=lbnodeagent -o yaml | grep "image:"

# Allocate a sample service:
kubectl apply -f deployments/samples/local-servicegroup.yaml   # edit pool subnet first
kubectl create deployment nginx --image=nginx
kubectl apply -f deployments/samples/sample-nginx-lb.yaml
kubectl get svc nginx                                          # EXTERNAL-IP populated

# kubectl-purelb plugin checks (catch the BGPNodeStatus class of latent bugs
# that v0.16.1-v0.16.3 shipped):
kubectl purelb status                                          # returns data, no errors
kubectl purelb pools                                           # shows the pool
kubectl get bgpnodestatus -A                                   # one row per node (proves
                                                               # the bundled k8gobgp is
                                                               # writing the CRD that the
                                                               # plugin reads)
```

Tear down before the next test:

```bash
kubectl delete -f deployments/install-${NEW}.yaml
kubectl delete -f deployments/install-crds-${NEW}.yaml
```

### Smoke test B — Helm install (local tgz)

The chart `.tgz` produced by `make helm` is the exact byte stream that
will land in the OCI registry and the classic-repo index post-release.
Pre-tag, install it directly from disk to verify the chart contents
before any publishing happens. The container images the chart references
do need to be pullable, so build and push them under an `rc1` tag first.

```bash
export KO_DOCKER_REPO=ghcr.io/purelb/purelb
export TAG=${NEW}-rc1
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/allocator
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/lbnodeagent
SUFFIX=$TAG REGISTRY_IMAGE=ghcr.io/purelb/purelb make helm
# Now ./purelb-${TAG}.tgz contains a chart whose values.yaml references
# the rc1 images we just pushed.

helm install --create-namespace -n purelb-system purelb \
  ./purelb-${TAG}.tgz
kubectl -n purelb-system get pods                              # all Running

# CRITICAL: the v0.16.5 release was cut because this assertion was
# never run before. The Helm RBAC was missing bgpnodestatuses.
kubectl get clusterrole purelb:k8gobgp -o yaml | grep -A1 bgpnodestatuses
# Expected: bgpnodestatuses AND bgpnodestatuses/status both present.

# Then run the sample-service + plugin checks:
kubectl apply -f deployments/samples/local-servicegroup.yaml
kubectl create deployment nginx --image=nginx
kubectl apply -f deployments/samples/sample-nginx-lb.yaml
kubectl get svc nginx                                          # EXTERNAL-IP populated
kubectl purelb status                                          # no errors
kubectl get bgpnodestatus -A                                   # one row per node
```

Tear down:

```bash
helm uninstall purelb -n purelb-system
kubectl delete crd servicegroups.purelb.io lbnodeagents.purelb.io \
  configs.bgp.purelb.io bgpnodestatuses.bgp.purelb.io
kubectl delete ns purelb-system
```

If anything is empty, erroring, or missing in either of the two
pre-tag smoke tests, **STOP**. The release ships a bug.

### Cleanup of rc1 artifacts

After the pre-tag smoke tests pass, delete the rc1 container image
versions from ghcr.io so they don't clutter the registry:

```bash
gh api -X DELETE /orgs/purelb/packages/container/purelb%2Fallocator/versions/<id-of-rc1>
gh api -X DELETE /orgs/purelb/packages/container/purelb%2Flbnodeagent/versions/<id-of-rc1>
```

(Use `gh api /orgs/purelb/packages/container/<pkg>/versions` to find IDs.
No chart cleanup needed since pre-tag smoke uses a local tgz, never a
published chart.)

## Land the changes

- [ ] Commit on `release/$NEW` branch.
- [ ] Push, open PR, wait for the `test` job (which now includes
      `check-deps` and `check-helm-rbac-source`), merge.
- [ ] `git checkout main && git pull && git tag $NEW <merge-sha> && git push origin $NEW`
      — pin to the merge SHA, not HEAD, to avoid racing other merges.

## CI auto-trigger chain

Tag push fires:
1. `ci.yml` (5 jobs, ~6 min): `test` → `build-images` → `build-plugin`
   → `build-manifests` → `scan`
2. `helm-index.yml` (workflow_run after `ci.yml` success, ~12 sec):
   regenerates `website/static/charts/index.yaml`, commits to main
3. `build.yml` (workflow_run after `helm-index.yml` success, ~3 min):
   rebuilds Hugo site, deploys to GitHub Pages

Total ~10 minutes from tag push to published index. No manual intervention.

## Post-release verification

- [ ] `gh release view $NEW` shows the expected assets (install manifests,
      install-crds, plugin binaries × 6 platforms, chart .tgz, SHA256SUMS).
- [ ] Both arches resolve:
      `crane manifest --platform linux/amd64 ghcr.io/purelb/purelb/lbnodeagent:$NEW`
      and same for `linux/arm64`.
- [ ] Helm OCI chart resolves:
      `crane manifest oci://ghcr.io/purelb/purelb/charts/purelb:$NEW`.
- [ ] Helm index includes the new version:
      `curl -s https://purelb.github.io/purelb/charts/index.yaml | grep -A2 "version: ${NEW#v}"`.
- [ ] **On a clean test cluster, repeat all three install paths against
      the official tag** (now that the chart is in the classic-repo index):
      - [ ] manifest install: `kubectl apply -f install-crds-${NEW}.yaml`
            then `install-${NEW}.yaml`
      - [ ] Helm OCI install: `helm install ... oci://ghcr.io/purelb/purelb/charts/purelb --version $NEW`
      - [ ] Helm classic-repo install:
            `helm repo add purelb https://purelb.github.io/purelb/charts && helm install ... purelb/purelb --version $NEW`
      For each: assert `kubectl get clusterrole purelb:k8gobgp -o yaml | grep -A1 bgpnodestatuses`
      shows both `bgpnodestatuses` and `bgpnodestatuses/status`, allocate
      a sample service, and confirm `kubectl purelb status` and
      `kubectl get bgpnodestatus -A` return populated data. **The point
      of repeating the test post-release is to validate that what
      shipped is what was tested** — pre-tag rc1 testing used different
      image tags and a local tgz; post-release validates the real
      artifacts that users will pull.

## If something goes wrong

### CI partial failure after tag push

If CI fails partway (e.g., images push but `build-manifests` errors), the
GitHub release exists but is incomplete. Rollback:

```bash
gh release delete $NEW -R purelb/purelb --yes --cleanup-tag
# (If gh is older and lacks --cleanup-tag, run:
#   git push --delete origin $NEW && git tag -d $NEW
#  manually after the release delete.)
```

Fix the root cause on a new branch, merge, retag with a fresh PR.

### Auto-trigger chain stalls

`gh workflow run helm-index.yml` then `gh workflow run build.yml`. (Has
happened once historically; PR #46 fixed the underlying cause but keep
this in your back pocket.)

### Image deletion

If you cut a tag in error and want to undo image publishes too, delete
the relevant versions via:

```bash
gh api -X DELETE /orgs/purelb/packages/container/<pkg>/versions/<id>
```

Multi-arch indexes have child manifests that show as untagged versions —
deleting the index alone leaves orphan children. See the GHCR cleanup
playbook for the safe procedure (it requires `crane` to walk the index
and identify child digests before deletion).
