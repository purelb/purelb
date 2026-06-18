---
title: "Migration"
description: "Upgrade an existing PureLB v0.13 (or earlier) install to the current release: API v1 to v2, Helm and manifest paths."
weight: 80
---

This guide covers upgrading an existing PureLB **v0.13.x** install — the last
release published on GitLab, which uses the `purelb.io/v1` API — to the current
release, which uses `purelb.io/v2`. The same procedure applies to any pre-`v2`
install (v0.15.x or earlier).

If you are installing PureLB for the first time, **skip this guide** and follow
the normal install instructions.

Steps 1–3 are the same no matter how PureLB was installed. **Step 4 forks** into
a Helm path and a manifest path — follow the one matching your install. Helm was
the default in v0.13, so it is shown first. Step 5 onward is shared again.

## What changes between v1 and v2

### Namespace

| v0.13 (GitLab) | current release |
|----------------|-----------------|
| `purelb` | `purelb-system` |

Your ServiceGroup and LBNodeAgent CRs are **namespaced**, and the allocator only
watches its **own** namespace (`purelb-system`). Converted CRs must live in
`purelb-system`, not `purelb`.

### ServiceGroup

v2 requires you to choose the pool type explicitly (validation enforces exactly
one):

- `spec.local` — addresses on the **same subnet** as your nodes (announced via ARP/NDP)
- `spec.remote` — addresses on a **different subnet** (announced on `kube-lb0` for BGP/routing)

In v1, `spec.local` covered both same-subnet and routed addresses. **Decide per
ServiceGroup:** if the pool is on the same subnet as your nodes, use `local`; if
it is reached via routing (typically `/32`/`/128` aggregation + BGP), use `remote`.

Pool field names (`v4pool`, `v6pool`, `v4pools`, `v6pools`) are **unchanged** and
remain lowercase in v2. The install ships **no** ServiceGroup, so you always
migrate these.

### LBNodeAgent

| v1 field | v2 field |
|----------|----------|
| `spec.local.localint` | `spec.local.localInterface` |
| `spec.local.extlbint` | `spec.local.dummyInterface` |
| `spec.local.sendgarp: <bool>` | `spec.local.garpConfig.enabled: <bool>` |

> **You may not need to migrate the `LBNodeAgent` at all.** The install ships a
> default `LBNodeAgent` named `default` (localInterface `default`, dummyInterface
> `kube-lb0`, GARP enabled). If your v1 `LBNodeAgent` used only those defaults,
> **skip it**. Migrate it **only if you changed a setting** (a non-default
> interface, `sendgarp: false`, a custom `garpConfig`/`addressConfig`, or extra
> `interfaces`).

### Other changes (no per-resource action required)

- **Election**: Memberlist (UDP/TCP 7934) replaced by Kubernetes Lease-based
  election. Port 7934 can be closed in firewalls.
- **BGP**: the current release ships an **integrated k8gobgp sidecar** in the
  lbnodeagent DaemonSet (configured via the `BGPConfiguration` CRD), replacing the
  standalone routing software (BIRD/FRR) used with v0.13. **Migrating an existing
  BIRD/FRR configuration to the sidecar is beyond the scope of this guide.** If you
  want to keep your existing external BGP routing, install PureLB **without** the
  sidecar — see [Installing without the BGP sidecar](#installing-without-the-bgp-sidecar).
  Otherwise, create a [BGPConfiguration]({{< relref "/docs/configuration/bgp" >}})
  after upgrading to use the sidecar.

## Step 1 — Confirm the cluster and how PureLB was installed

The result decides which path you take at Step 4. Helm-managed objects carry the
`app.kubernetes.io/managed-by: Helm` label; the `lbnodeagent` DaemonSet is the
easiest place to check.

```bash
kubectl config current-context     # confirm you are on the right cluster

# Prints "Helm" for a Helm install, empty for a manifest install:
kubectl get daemonset lbnodeagent -n purelb \
  -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}{"\n"}'

# Cross-check: a Helm install appears here; a manifest install does not.
# Note the release NAME and NAMESPACE — you uninstall it in Step 4 (Helm).
helm list -A | grep purelb
```

- Output `Helm` (and a release in `helm list`) → use **[Step 4 (Helm)](#step-4-helm)**.
- Empty output (nothing in `helm list`) → it was a manifest install → use
  **[Step 4 (manifest)](#step-4-manifest)**.

The source namespace is assumed to be `purelb`; adjust the commands if yours
differs.

## Step 2 — Back up your v1 resources

```bash
mkdir -p ./purelb-migration
kubectl get servicegroups.purelb.io -n purelb -o yaml > ./purelb-migration/servicegroups-v1-backup.yaml
kubectl get lbnodeagents.purelb.io  -n purelb -o yaml > ./purelb-migration/lbnodeagents-v1-backup.yaml
```

## Step 3 — Convert your resources to v2

The backups are each a `kind: List` (one entry per resource under `items`). Edit
the List files **in place, keeping the `List` wrapper**, so you convert and apply
every resource at once — however many ServiceGroups you have. For each entry
under `items` (see [What changes](#what-changes-between-v1-and-v2) for the field
mappings):

- set `apiVersion: purelb.io/v2`
- set `metadata.namespace: purelb-system`
- for **ServiceGroups**: choose `spec.local` or `spec.remote`
- for **LBNodeAgents**: apply the field renames — but first check whether you
  need it at all (skip note above)
- delete `status` and server-managed metadata (`resourceVersion`, `uid`,
  `creationTimestamp`, `generation`, `managedFields`)

You will `kubectl apply` these files in Step 5, after the new release is installed.

**`servicegroups-v2.yaml`** — one entry per pool (repeat the `items` block for
each ServiceGroup you have):

```yaml
apiVersion: v1
kind: List
items:
- apiVersion: purelb.io/v2
  kind: ServiceGroup
  metadata:
    name: default
    namespace: purelb-system
  spec:
    local:                       # or: remote
      v4pools:
      - subnet: 192.0.2.0/24
        pool: 192.0.2.150-192.0.2.160
        aggregation: default
- apiVersion: purelb.io/v2        # second ServiceGroup, if any
  kind: ServiceGroup
  metadata:
    name: bgp-pool
    namespace: purelb-system
  spec:
    remote:
      v4pools:
      - subnet: 203.0.113.0/24
        pool: 203.0.113.0/28
        aggregation: /32
```

**`lbnodeagents-v2.yaml`** — *only if you customized it* (otherwise skip):

```yaml
apiVersion: v1
kind: List
items:
- apiVersion: purelb.io/v2
  kind: LBNodeAgent
  metadata:
    name: default
    namespace: purelb-system
  spec:
    local:
      localInterface: default
      dummyInterface: kube-lb0
      garpConfig:
        enabled: false           # was sendgarp: false
```

## Step 4 — Remove v0.13 and install the new release

Follow **one** path, matching what you found in Step 1. Both end the same way:
the new release running in `purelb-system` with the v2 CRDs and a default
LBNodeAgent, ready for your converted resources in Step 5.

**Warning:** Removing the old node agent gracefully withdraws the VIPs from the
node interface before the new agent starts, avoiding two announcers contending
for the interface. Your LoadBalancer Services live in their own namespaces and
are not affected. As the Services are not removed, they retain their addresses but will not be reachable until the new release is installed.


### Step 4 (Helm)

Helm **cannot migrate the CRDs**: the chart ships them in its `crds/` directory,
which Helm installs only on first `helm install` and never upgrades or deletes.
So you uninstall the old release, delete the v1 CRDs by hand, then fresh-install.

```bash
# 1. Find the installed release, then uninstall it using the value from the NAME column
helm list -n purelb
helm uninstall <NAME> -n purelb --wait

# 2. helm uninstall leaves the namespace and the CRDs. Remove the namespace if it
#    was created for PureLB:
kubectl delete namespace purelb --wait=true

# 3. Delete the v1 CRDs — Helm will not. This clears status.storedVersions=["v1"]
#    so the v2 CRDs can install. Your data is safe in the Step 2 backup.
kubectl delete crd servicegroups.purelb.io lbnodeagents.purelb.io

# 4. Fresh install (installs v2 CRDs + workloads + a default LBNodeAgent).

# First, remove the stale v0.13 GitLab Helm repo if it is still in your repo list
# — it is no longer valid and leaving it is confusing. Do this for both options;
# the OCI option does not use a repo, but the dead entry should still go:
helm repo remove purelb 2>/dev/null || true

# Then install with EITHER option A or B.

# Option A — chart repository: add the current repo, then install from it
helm repo add purelb https://purelb.io/charts
helm repo update purelb
helm install --create-namespace --namespace=purelb-system purelb \
    purelb/purelb

# Option B — OCI registry (Helm 3.8+), no repo needed:
helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.16.6

# Wait for the rollout (whichever option you ran):
kubectl rollout status deploy/allocator -n purelb-system --timeout=120s
kubectl rollout status ds/lbnodeagent  -n purelb-system --timeout=120s
```

Notes for the Helm path:

- **No in-place `helm upgrade`** across the v1→v2 CRD boundary — it would skip the
  CRDs. Uninstall + delete CRDs + fresh install, as above.
- **Ownership conflicts**: the fresh install succeeds cleanly only if the old
  release's resources are gone. A lingering old release (different name/namespace)
  causes `invalid ownership metadata` errors — uninstall it first.

Continue to [Step 5](#step-5--apply-your-converted-v2-resources).

### Step 4 (manifest)

```bash
# 1. Remove the old install
kubectl delete namespace purelb --wait=true

# 2. Delete the v1 CRDs. This clears status.storedVersions=["v1"] so the v2 CRDs
#    can install. Your data is safe in the Step 2 backup.
kubectl delete crd servicegroups.purelb.io lbnodeagents.purelb.io

# 3. Install the v2 CRDs (now a clean create)
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.6/install-crds-v0.16.6.yaml
kubectl get crd servicegroups.purelb.io lbnodeagents.purelb.io \
  -o jsonpath='{range .items[*]}{.metadata.name}{" versions="}{.spec.versions[*].name}{" stored="}{.status.storedVersions}{"\n"}{end}'
# expect: versions=v2 stored=["v2"]

# 4. Install the workloads (creates purelb-system + a default LBNodeAgent)
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.6/install-v0.16.6.yaml
kubectl rollout status deploy/allocator -n purelb-system --timeout=120s
kubectl rollout status ds/lbnodeagent  -n purelb-system --timeout=120s
```

Continue to [Step 5](#step-5--apply-your-converted-v2-resources).

## Step 5 — Apply your converted v2 resources

Same for both paths. Apply **after** Step 4 so a customized LBNodeAgent overrides
the default that the install shipped.

```bash
kubectl apply -f ./purelb-migration/servicegroups-v2.yaml
# only if you customized the LBNodeAgent:
kubectl apply -f ./purelb-migration/lbnodeagents-v2.yaml
```

## Step 6 — Verify

Confirm the components are running and your resources are present:

```bash
kubectl get pods -n purelb-system               # allocator + lbnodeagent Running/Ready
kubectl get servicegroup,lbnodeagent -n purelb-system
kubectl get svc -A | grep LoadBalancer          # existing services keep their addresses
```

The allocator log is **not** a useful health check here. It carries benign
`unknown LoadBalancer IP: no pool found` lines from the window before the
ServiceGroup existed (see
[Notes](#existing-services-keep-their-address-before-the-servicegroup-is-applied)),
and those lines stay in the log afterward. To confirm allocation works cleanly,
create a **new** test Service and check it gets an address from your pool:

```bash
kubectl create deployment test --image=nginx
kubectl expose deployment test --type=LoadBalancer --port=80
kubectl get svc test                            # EXTERNAL-IP populates from your pool
kubectl delete svc test; kubectl delete deployment test
```

## Installing without the BGP sidecar

The current release installs the k8gobgp sidecar by default. If you are keeping
your existing external BGP routing (BIRD/FRR) — migrating it to the sidecar is out
of scope here — install PureLB without the sidecar by adjusting the install in
**Step 4**. Everything else in the procedure is unchanged, and no
`BGPConfiguration` is needed: remote-pool VIPs are still placed on `kube-lb0`,
where your existing BIRD/FRR imports and announces them exactly as under v0.13.

**Helm** — add `--set gobgp.enabled=false` to the install:

```bash
helm install --create-namespace --namespace=purelb-system purelb \
    oci://ghcr.io/purelb/purelb/charts/purelb --version v0.16.6 \
    --set gobgp.enabled=false
```

**Manifest** — use the `-nobgp` release artifacts in place of the standard ones
(these omit the `bgp.purelb.io` CRDs and the sidecar):

```bash
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.6/install-crds-nobgp-v0.16.6.yaml
kubectl apply -f https://github.com/purelb/purelb/releases/download/v0.16.6/install-nobgp-v0.16.6.yaml
```

## Notes

### Existing services keep their address before the ServiceGroup is applied

A LoadBalancer Service stores its assigned IP in its own `status` and carries the
`purelb.io/allocated-by: PureLB` annotation. When the new allocator starts, it
re-announces that address from the Service object — so the IP can reappear on the
node **before** the matching ServiceGroup exists (Step 5). Until it does, the
allocator logs `unknown LoadBalancer IP: no pool found` for that address; this is
benign and clears once the pool is defined. The IP does not change.


### kubectl default namespace

If your kubeconfig context's default namespace was `purelb`, it no longer exists
after Step 4. Set it to a valid namespace to avoid `namespace not found` errors:

```bash
kubectl config set-context --current --namespace=purelb-system
```

## Rollback

If you need to revert:

```bash
kubectl delete crd servicegroups.purelb.io lbnodeagents.purelb.io
# go to https://gitlab.com/purelb/purelb
# redeploy the v0.13 allocator/lbnodeagent (manifest or Helm, as before)
kubectl apply -f ./purelb-migration/servicegroups-v1-backup.yaml
kubectl apply -f ./purelb-migration/lbnodeagents-v1-backup.yaml
```
