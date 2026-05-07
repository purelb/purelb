# Releasing PureLB

This document is the canonical release procedure for PureLB. The primary
defense against drift is the `make check-deps` CI gate; this checklist is
the secondary defense, covering what the gate cannot enforce (procedural
drift, missed doc bumps, post-tag rollback).

Run through every section in order. Each item exists because a prior
release missed it.

## Pre-flight (hard gates)

- [ ] `git status` clean on `main`, `git pull` up to date.
- [ ] `make check` passes locally. **This includes `make check-deps`. If
      `check-deps` fails, you have a CRD/version mismatch and must NOT
      proceed — fix it via the [Dependency review](#dependency-review-k8gobgp)
      below.**
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
make check                              # vet + race tests + check-deps
SUFFIX=$NEW make manifest               # render the install manifest
SUFFIX=$NEW make install-manifest       # render the standalone install
SUFFIX=$NEW make helm                   # package the chart
grep "k8gobgp:" deployments/install-${NEW}.yaml   # confirms new tag in output
```

**Required (not "if handy"): clean-cluster smoke test.** Per project
convention, pre-existing CRDs/state mask real ordering bugs. Until CI has
an automated end-to-end smoke test, this manual run is the only thing
standing between a known bug and your users.

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

If anything is empty, erroring, or missing, **STOP**. The release ships a
bug.

## Land the changes

- [ ] Commit on `release/$NEW` branch.
- [ ] Push, open PR, wait for the `test` job (which now includes
      `check-deps`), merge.
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
- [ ] On a clean test cluster: install via the README URL, allocate a
      sample service, confirm `kubectl purelb status` and
      `kubectl get bgpnodestatus -A` return populated data.

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
