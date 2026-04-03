# Plan: Split PureLB Installation — Base vs. With k8gobgp

## Context

PureLB currently ships a single installation variant. Users who want BGP route announcement (via k8gobgp) must set it up manually and separately. The goal is to provide official "with k8gobgp" variants of all three installation mechanisms — kustomize install-manifest, kustomize manifest-with-samples, and Helm chart — while leaving the existing base variants 100% unchanged.

k8gobgp (https://github.com/purelb/k8gobgp) is a Kubernetes controller that manages GoBGP daemon(s) via a `BGPConfiguration` CRD (`bgp.purelb.io`). It will run as a **sidecar** in the existing `lbnodeagent` DaemonSet pod, which already has `hostNetwork: true`, so it inherits the host network namespace automatically. Configuration is declarative via Kubernetes CRs — no ConfigMap needed.

## Key Facts About k8gobgp

- **Image**: `ghcr.io/purelb/k8gobgp:v0.2.2` (pinned; separate release cadence from PureLB)
- **Configuration**: Via `BGPConfiguration` CRD (`bgp.purelb.io`), NOT a ConfigMap
- **Capabilities required**: `NET_ADMIN`, `NET_BIND_SERVICE`, `NET_RAW`
- **Environment variables**: `NODE_NAME`, `POD_NAME`, `POD_NAMESPACE` (downward API)
- **Ports**: 179 (BGP), 7473 (metrics), 7474 (health probes) — defaults changed in k8gobgp repo (see prerequisite below)
- **Unix socket**: `/var/run/gobgp/gobgp.sock` — shared `emptyDir` volume within the pod
- **RBAC**: Needs its own ClusterRole (bgp.purelb.io/configs, nodes, secrets, pods, events)
- **BGPConfiguration CRD** must be installed alongside the PureLB CRDs

## Deployment Prerequisites

These must be documented at install time (not enforced by manifests):

1. **k8gobgp repo must be updated first** — change `--metrics-bind-address` default from `:8080` to `:7473` and `--health-probe-bind-address` default from `:8081` to `:7474` in the k8gobgp source. Cut a new release and update `GOBGP_TAG` here before implementing. Port 8080 is unsafe for `hostNetwork: true` pods; 7473/7474 keep all PureLB-related ports in a consistent range alongside lbnodeagent's 7472.
2. **Port 179 must be available** on every node — no other BGP daemon (Calico BGP, BIRD, FRR, Cilium BGP) may be listening on port 179. k8gobgp binds to `0.0.0.0:179` in the host network namespace; `bind()` will fail with EADDRINUSE if another process owns that port.
3. **Firewall/iptables must allow port 179** ingress and egress between cluster nodes and the upstream BGP router. Verify with `nc -zv <router-ip> 179` from a node.
4. **Upstream router must be configured** to peer with each node's IP at the agreed ASN before BGP sessions can establish.
5. **ECMP must be enabled** on the upstream router if multi-node load distribution is desired.

## Route Filtering

Netlink import is **disabled by default** in k8gobgp. Routes are only imported when `netlinkImport.enabled: true` is set in the BGPConfiguration CR, and only from interfaces listed in `interfaceList`. The default sample CR explicitly sets `interfaceList: ["kube-lb0"]`, so only PureLB VIPs are advertised. Importing routes from other interfaces (e.g., eth0, cluster CIDRs, default route) requires deliberately adding those interfaces to the list — there is no route leakage risk with the default configuration.

## RBAC Strategy for the Sidecar

A Kubernetes pod has exactly **one** ServiceAccount. k8gobgp running in the lbnodeagent pod gains k8s API access via an additional binding — clean and additive:

1. Create a **new** `ClusterRole purelb:k8gobgp` with k8gobgp's specific rules
2. Create a **new** `ClusterRoleBinding purelb:k8gobgp` binding that role to the existing `lbnodeagent` ServiceAccount
3. No modifications to the existing `purelb:lbnodeagent` ClusterRole or ClusterRoleBinding

---

## Implementation Plan

### Option Chosen: Kustomize Component + Single Helm Chart with Feature Flag

**Rejected alternatives:**
- Two separate Helm charts — duplicates all templates, high maintenance cost
- Simple overlay (no Component) — cannot be composed into custom user overlays; patch duplication across `with-gobgp/` and `samples-with-gobgp/`

---

### 1. New Directory/File Structure

```
deployments/
  components/
    gobgp/
      kustomization.yaml           # Component declaration (kind: Component)
      gobgp-patch.yaml             # Strategic merge patch: adds sidecar to lbnodeagent DaemonSet
      gobgp-rbac.yaml              # ClusterRole + ClusterRoleBinding for k8gobgp
      gobgp-bgpconfig-crd.yaml     # BGPConfiguration CRD (fetched via make fetch-gobgp-crd)
  with-gobgp/
    kustomization.yaml             # Overlay: default + gobgp component
  samples-with-gobgp/
    kustomization.yaml             # Overlay: samples + gobgp component
    sample-bgpconfig.yaml          # Sample BGPConfiguration CR (IPv4 + IPv6)

build/helm/purelb/
  templates/
    gobgp-rbac.yaml                # New: conditional RBAC for k8gobgp
    gobgp-metrics-service.yaml     # New: conditional metrics Service (gated on gobgp.enabled AND Prometheus.lbnodeagent.Metrics.enabled)
    (daemonset.yaml modified)      # Add conditional sidecar container + emptyDir volume
  values.yaml                      # Add gobgp: stanza
```

Makefile: two new build targets + one CRD-fetch utility target.
CI: `.github/workflows/ci.yml` updated for gobgp release artifacts.

---

### 2. `deployments/components/gobgp/kustomization.yaml`

```yaml
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component

resources:
- gobgp-bgpconfig-crd.yaml
- gobgp-rbac.yaml

patches:
- path: gobgp-patch.yaml
  target:
    kind: DaemonSet
    name: lbnodeagent
```

`kind: Component` (not `Kustomization`) is required. Components cannot be built standalone — they only apply when composed into an overlay.

---

### 3. `deployments/components/gobgp/gobgp-patch.yaml`

Strategic merge patch. Kubernetes merges `containers:` lists by `name` key — the new `k8gobgp` container appends cleanly without touching the existing `lbnodeagent` container. The `volumes:` key is also additive.

**Note on image tags**: The image here is the default pinned version. The Makefile runs `kustomize edit set image ghcr.io/purelb/k8gobgp=...` in the overlay's kustomization.yaml, which adds an `images:` override that Kustomize applies by matching the image name (ignoring the tag in the patch). This is the standard Kustomize image override mechanism — the overlay's `images:` stanza takes precedence over the patch's hardcoded tag at build time.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: lbnodeagent
  namespace: purelb-system
spec:
  template:
    spec:
      volumes:
      - name: gobgp-socket
        emptyDir: {}
      containers:
      - name: k8gobgp
        image: ghcr.io/purelb/k8gobgp:v0.2.2
        imagePullPolicy: IfNotPresent
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        ports:
        - containerPort: 7473
          name: gobgp-metrics
        - containerPort: 7474
          name: gobgp-health
        startupProbe:
          httpGet:
            path: /readyz
            port: 7474
          initialDelaySeconds: 5
          periodSeconds: 5
          failureThreshold: 12   # 60s total startup window
        livenessProbe:
          httpGet:
            path: /healthz
            port: 7474
        readinessProbe:
          httpGet:
            path: /readyz
            port: 7474
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            - NET_BIND_SERVICE
            - NET_RAW
            drop:
            - ALL
          readOnlyRootFilesystem: true
        resources:
          requests:
            cpu: 250m
            memory: 128Mi
          limits:
            cpu: 1000m
            memory: 512Mi
        volumeMounts:
        - name: gobgp-socket
          mountPath: /var/run/gobgp
```

---

### 4. `deployments/components/gobgp/gobgp-rbac.yaml`

Additive RBAC — binds to the existing `lbnodeagent` ServiceAccount, no changes to existing roles.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  labels:
    app: purelb
  name: purelb:k8gobgp
rules:
- apiGroups:
  - bgp.purelb.io
  resources:
  - configs
  - configs/finalizers
  - configs/status
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ''
  resources:
  - nodes
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - ''
  resources:
  - pods
  verbs:
  - patch
- apiGroups:
  - ''
  resources:
  - secrets
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - ''
  resources:
  - events
  verbs:
  - create
  - patch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  labels:
    app: purelb
  name: purelb:k8gobgp
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: purelb:k8gobgp
subjects:
- kind: ServiceAccount
  name: lbnodeagent
  namespace: purelb-system
```

---

### 5. `deployments/components/gobgp/gobgp-bgpconfig-crd.yaml`

Generated by `make fetch-gobgp-crd` (see Makefile section). Do not edit manually. Contains only the `CustomResourceDefinition` for `bgp.purelb.io` extracted from the k8gobgp v0.2.2 release.

---

### 6. `deployments/with-gobgp/kustomization.yaml`

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../default

components:
- ../components/gobgp
```

---

### 7. `deployments/samples-with-gobgp/kustomization.yaml`

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../samples
- sample-bgpconfig.yaml

components:
- ../components/gobgp
```

---

### 8. `deployments/samples-with-gobgp/sample-bgpconfig.yaml`

Includes both IPv4 and IPv6 address families. `routerId` left empty for auto-detection; **operators should explicitly set it to the node's BGP-facing IP** if nodes have multiple network interfaces to avoid ambiguity.

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: default
  namespace: purelb-system
spec:
  global:
    config:
      # Use a private ASN from range 64512-65534.
      # All nodes share this ASN (eBGP topology: upstream router uses a different ASN).
      as: 65000
      # Leave empty for auto-detection from NODE_NAME, OR set explicitly to node's
      # BGP-facing IP to avoid ambiguity on multi-homed nodes (recommended).
      routerId: ""

  # netlinkImport controls which kernel routes are imported into BGP for advertisement.
  # REQUIRED: without this block, k8gobgp starts but advertises NO routes.
  # For PureLB, restrict to kube-lb0 — the dummy interface where VIPs are placed.
  # Importing from other interfaces is a custom configuration, not the default use case.
  netlinkImport:
    enabled: true
    interfaceList:
    - "kube-lb0"

  neighbors:
  - config:
      # REQUIRED: Replace with your upstream BGP router's IP address.
      neighborAddress: 192.0.2.1
      # REQUIRED: Replace with your upstream router's ASN (must differ from 'as' above for eBGP).
      peerAs: 65001
    # Enable both IPv4 and IPv6 address families.
    # Remove the ipv6-unicast entry if you only use IPv4 VIPs.
    afiSafis:
    - config:
        family:
          afi: AFI_IP
          safi: SAFI_UNICAST
    - config:
        family:
          afi: AFI_IP6
          safi: SAFI_UNICAST
```

---

### 9. Makefile Additions

**New variables** (add near top with other defaults):
```makefile
GOBGP_IMAGE ?= ghcr.io/purelb/k8gobgp
GOBGP_TAG   ?= v0.2.2
```

**New targets** (insert after existing `install-manifest` and `manifest` targets):

```makefile
.ONESHELL:
.PHONY: install-manifest-gobgp
install-manifest-gobgp: CACHE != mktemp
install-manifest-gobgp: crd  ## Generate standalone install.yaml with k8gobgp sidecar
	cd deployments/with-gobgp
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX} ghcr.io/purelb/k8gobgp=${GOBGP_IMAGE}:${GOBGP_TAG}
	$(KUSTOMIZE) build . > ../install-gobgp-${MANIFEST_SUFFIX}.yaml
	cp ${CACHE} kustomization.yaml

.ONESHELL:
.PHONY: manifest-gobgp
manifest-gobgp: CACHE != mktemp
manifest-gobgp:  ## Generate deployment manifest with samples and k8gobgp sidecar
	cd deployments/samples-with-gobgp
	cp kustomization.yaml ${CACHE}
	$(KUSTOMIZE) edit set image ghcr.io/purelb/purelb/allocator=${REGISTRY_IMAGE}/allocator:${SUFFIX} ghcr.io/purelb/purelb/lbnodeagent=${REGISTRY_IMAGE}/lbnodeagent:${SUFFIX} ghcr.io/purelb/k8gobgp=${GOBGP_IMAGE}:${GOBGP_TAG}
	$(KUSTOMIZE) build . > ../${PROJECT}-gobgp-${MANIFEST_SUFFIX}.yaml
	cp ${CACHE} kustomization.yaml

.PHONY: fetch-gobgp-crd
fetch-gobgp-crd:  ## Fetch BGPConfiguration CRD from k8gobgp ${GOBGP_TAG} release
	curl -fsSL https://github.com/purelb/k8gobgp/releases/download/${GOBGP_TAG}/install.yaml \
	  | go run sigs.k8s.io/kustomize/kustomize/v4@v4.5.2 cfg grep "kind=CustomResourceDefinition" \
	  > deployments/components/gobgp/gobgp-bgpconfig-crd.yaml
```

**Update `helm` target** — add CRD fetch step after copying PureLB CRDs:
```makefile
helm:  ## Package PureLB using Helm
	rm -rf build/build
	mkdir -p build/build
	cp -r build/helm/purelb build/build/
	cp deployments/crds/purelb.io_*.yaml build/build/purelb/crds
	# Bundle BGPConfiguration CRD so `gobgp.enabled=true` works without a separate CRD install
	curl -fsSL https://github.com/purelb/k8gobgp/releases/download/${GOBGP_TAG}/install.yaml \
	  | go run sigs.k8s.io/kustomize/kustomize/v4@v4.5.2 cfg grep "kind=CustomResourceDefinition" \
	  > build/build/purelb/crds/bgp.purelb.io_bgpconfigurations.yaml
	cp README.md build/build/purelb
	sed \
	--expression="s~DEFAULT_REPO~${REGISTRY_IMAGE}~" \
	--expression="s~DEFAULT_TAG~${SUFFIX}~" \
	build/helm/purelb/values.yaml > build/build/purelb/values.yaml
	${HELM} package \
	--version "${SUFFIX}" --app-version "${SUFFIX}" \
	build/build/purelb
```

Output files:
- `deployments/install-gobgp-${SUFFIX}.yaml`
- `deployments/${PROJECT}-gobgp-${SUFFIX}.yaml`

---

### 10. CI/CD: `.github/workflows/ci.yml` Changes

Add two steps immediately after the existing "Generate install manifest" step:

```yaml
      - name: Generate manifests (with k8gobgp)
        env:
          SUFFIX: ${{ github.ref_name }}
          MANIFEST_SUFFIX: ${{ github.ref_name }}
          GOBGP_TAG: v0.2.2
        run: make manifest-gobgp

      - name: Generate install manifest (with k8gobgp)
        env:
          SUFFIX: ${{ github.ref_name }}
          MANIFEST_SUFFIX: ${{ github.ref_name }}
          GOBGP_TAG: v0.2.2
        run: make install-manifest-gobgp
```

Update the "Generate checksums" step:
```yaml
      - name: Generate checksums
        run: |
          sha256sum purelb-${{ github.ref_name }}.tgz > SHA256SUMS
          sha256sum deployments/purelb-${{ github.ref_name }}.yaml >> SHA256SUMS
          sha256sum deployments/install-${{ github.ref_name }}.yaml >> SHA256SUMS
          sha256sum deployments/purelb-gobgp-${{ github.ref_name }}.yaml >> SHA256SUMS
          sha256sum deployments/install-gobgp-${{ github.ref_name }}.yaml >> SHA256SUMS
```

Update the "Create Release" step files list:
```yaml
      - name: Create Release
        uses: softprops/action-gh-release@v2
        with:
          files: |
            purelb-${{ github.ref_name }}.tgz
            deployments/purelb-${{ github.ref_name }}.yaml
            deployments/install-${{ github.ref_name }}.yaml
            deployments/purelb-gobgp-${{ github.ref_name }}.yaml
            deployments/install-gobgp-${{ github.ref_name }}.yaml
            SHA256SUMS
          generate_release_notes: true
```

---

### 11. Helm Chart — `values.yaml` Additions

Add at end of `build/helm/purelb/values.yaml`:

```yaml
# k8gobgp sidecar — per-node BGP route announcement for remote VIPs.
# When enabled, k8gobgp runs as a sidecar in the lbnodeagent DaemonSet pod.
# It shares hostNetwork:true and announces VIP routes to upstream BGP routers.
#
# Prerequisites:
#   - Port 179 must be free on every node (no other BGP daemon running).
#   - Firewall must allow TCP 179 between nodes and upstream router.
#   - Apply a BGPConfiguration CR after install to configure peers/ASNs.
#   - Upstream router must be configured to peer with each node IP.
gobgp:
  enabled: false

  image:
    repository: ghcr.io/purelb/k8gobgp
    tag: v0.2.2
    pullPolicy: IfNotPresent

  containerSecurityContext:
    allowPrivilegeEscalation: false
    capabilities:
      add:
      - NET_ADMIN
      - NET_BIND_SERVICE
      - NET_RAW
      drop:
      - ALL
    readOnlyRootFilesystem: true

  resources:
    requests:
      cpu: 250m
      memory: 128Mi
    limits:
      cpu: 1000m
      memory: 512Mi
```

---

### 12. Helm Template — `daemonset.yaml` Changes

Two additions inside the existing pod `spec:` block:

**a) After the `tolerations` block, add the shared emptyDir volume:**
```yaml
      {{- if .Values.gobgp.enabled }}
      volumes:
      - name: gobgp-socket
        emptyDir: {}
      {{- end }}
```

**b) After the closing of the lbnodeagent container spec, add the sidecar:**
```yaml
      {{- if .Values.gobgp.enabled }}
      - name: k8gobgp
        image: "{{ .Values.gobgp.image.repository }}:{{ .Values.gobgp.image.tag }}"
        imagePullPolicy: {{ .Values.gobgp.image.pullPolicy }}
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        ports:
        - containerPort: 7473
          name: gobgp-metrics
        - containerPort: 7474
          name: gobgp-health
        startupProbe:
          httpGet:
            path: /readyz
            port: 7474
          initialDelaySeconds: 5
          periodSeconds: 5
          failureThreshold: 12
        livenessProbe:
          httpGet:
            path: /healthz
            port: 7474
        readinessProbe:
          httpGet:
            path: /readyz
            port: 7474
        {{- with .Values.gobgp.containerSecurityContext }}
        securityContext:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        resources:
          {{- with .Values.gobgp.resources }}
          {{- toYaml . | nindent 10 }}
          {{- end }}
        volumeMounts:
        - name: gobgp-socket
          mountPath: /var/run/gobgp
      {{- end }}
```

---

### 13. Helm Template — `gobgp-rbac.yaml` (new file)

```yaml
{{- if .Values.gobgp.enabled }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "purelb.fullname" . }}:k8gobgp
  labels:
    {{- include "purelb.labels" . | nindent 4 }}
rules:
- apiGroups: ["bgp.purelb.io"]
  resources: ["configs", "configs/finalizers", "configs/status"]
  verbs: ["create","delete","get","list","patch","update","watch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get","list","watch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["patch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get","list","watch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create","patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "purelb.fullname" . }}:k8gobgp
  labels:
    {{- include "purelb.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "purelb.fullname" . }}:k8gobgp
subjects:
- kind: ServiceAccount
  name: lbnodeagent
  namespace: {{ .Release.Namespace }}
{{- end }}
```

---

### 14. Helm Template — `gobgp-metrics-service.yaml` (new file)

Gated on **both** `gobgp.enabled` AND `Prometheus.lbnodeagent.Metrics.enabled` — consistent with the existing `service-metrics-lbnodeagent.yaml` pattern. In kustomize, lbnodeagent metrics use pod annotation-based discovery only (no Service); we follow the same convention for k8gobgp — no metrics Service in the kustomize path.

```yaml
{{- if and .Values.gobgp.enabled .Values.Prometheus.lbnodeagent.Metrics.enabled }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ include "purelb.fullname" . }}-k8gobgp-metrics
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "purelb.labels" . | nindent 4 }}
    app.kubernetes.io/component: k8gobgp
spec:
  selector:
    {{- include "purelb.selectorLabels" . | nindent 4 }}
    app.kubernetes.io/component: lbnodeagent
  type: ClusterIP
  ports:
  - name: metrics
    port: 7473
    targetPort: 7473
    protocol: TCP
{{- end }}
```

---

## Implementation Order

**Step 1** (prerequisite): Update k8gobgp repo — change `--metrics-bind-address` default to `:7473` and `--health-probe-bind-address` to `:7474`, cut a new release, update `GOBGP_TAG` here.

**Step 2**: `make fetch-gobgp-crd` — generates `deployments/components/gobgp/gobgp-bgpconfig-crd.yaml`.

**Steps 3+**: Implement remaining files per the plan above.

---

## Critical Files to Modify

| File | Change |
|------|--------|
| `Makefile` | Add `GOBGP_IMAGE`, `GOBGP_TAG` vars; add `install-manifest-gobgp`, `manifest-gobgp`, `fetch-gobgp-crd` targets; update `helm` target to bundle BGPConfiguration CRD |
| `build/helm/purelb/values.yaml` | Add `gobgp:` stanza |
| `build/helm/purelb/templates/daemonset.yaml` | Add conditional sidecar container + emptyDir volume |
| `.github/workflows/ci.yml` | Add gobgp manifest generation + checksums + release artifact upload |

## New Files to Create

| File | Purpose |
|------|---------|
| `deployments/components/gobgp/kustomization.yaml` | Kustomize Component declaration |
| `deployments/components/gobgp/gobgp-patch.yaml` | Strategic merge patch for lbnodeagent DaemonSet |
| `deployments/components/gobgp/gobgp-rbac.yaml` | ClusterRole + ClusterRoleBinding for k8gobgp |
| `deployments/components/gobgp/gobgp-bgpconfig-crd.yaml` | BGPConfiguration CRD (generated by `make fetch-gobgp-crd`) |
| `deployments/with-gobgp/kustomization.yaml` | Overlay: default + gobgp component |
| `deployments/samples-with-gobgp/kustomization.yaml` | Overlay: samples + gobgp component |
| `deployments/samples-with-gobgp/sample-bgpconfig.yaml` | Sample BGPConfiguration CR (IPv4 + IPv6) |
| `build/helm/purelb/templates/gobgp-rbac.yaml` | Helm conditional RBAC template |
| `build/helm/purelb/templates/gobgp-metrics-service.yaml` | Helm conditional metrics Service (requires gobgp.enabled AND Prometheus.lbnodeagent.Metrics.enabled) |

## Verification

### Offline (no cluster required)
1. `make fetch-gobgp-crd` — downloads CRD; verify file is valid YAML with `kind: CustomResourceDefinition`
2. `make install-manifest` — output **identical** to pre-change baseline (regression)
3. `make manifest` — output **identical** to pre-change baseline (regression)
4. `make install-manifest-gobgp` — output contains: BGPConfiguration CRD, k8gobgp ClusterRole/Binding, lbnodeagent DaemonSet with 2 containers
5. `make manifest-gobgp` — same as above plus sample ServiceGroup, sample BGPConfiguration CR
6. `make helm` — `helm lint build/build/purelb` passes; `helm template . --set gobgp.enabled=false` identical to baseline; `helm template . --set gobgp.enabled=true` contains 2 containers, RBAC, metrics Service

### On test cluster — Kustomize install-manifest-gobgp (prox-purelb2)
7. `kubectl apply -f deployments/install-gobgp-${SUFFIX}.yaml` — all pods reach Running state
8. `kubectl get pods -n purelb-system -l component=lbnodeagent -o jsonpath='{.items[*].spec.containers[*].name}'` — shows `lbnodeagent k8gobgp` for every pod
9. `kubectl exec -n purelb-system <pod> -c k8gobgp -- curl -sf http://localhost:7474/readyz` — returns 200
10. `kubectl get clusterrole purelb:k8gobgp && kubectl get clusterrolebinding purelb:k8gobgp` — exist
11. Apply sample BGPConfiguration CR — verify BGP sessions establish with FRR router (`vtysh -c 'show bgp summary'`)
12. **Route filtering check**: `gobgp global rib` from k8gobgp container — confirm only kube-lb0 routes present
13. Deploy a test LoadBalancer Service — confirm VIP route appears on FRR router; delete service — confirm route withdraws
14. **readOnlyRootFilesystem test**: check k8gobgp logs for any "read-only file system" errors over 5 minutes of operation
15. Clean up: `kubectl delete -f deployments/install-gobgp-${SUFFIX}.yaml`

### On test cluster — Kustomize manifest-gobgp (prox-purelb2)
16. `kubectl apply -f deployments/${PROJECT}-gobgp-${SUFFIX}.yaml` — all pods Running, sample BGPConfiguration CR applied
17. Verify same pod/container/RBAC checks as items 8-10
18. Verify sample BGPConfiguration CR is present: `kubectl get bgpconfigurations.bgp.purelb.io -n purelb-system`
19. Verify sample ServiceGroup is present: `kubectl get servicegroups.purelb.io -n purelb-system`
20. Deploy a test LoadBalancer Service — confirm VIP allocation and BGP route advertisement
21. Clean up: `kubectl delete -f deployments/${PROJECT}-gobgp-${SUFFIX}.yaml`

### On test cluster — Helm with gobgp.enabled=true (prox-purelb2)
22. `helm install --create-namespace --namespace=purelb-system purelb ./build/build/purelb --set gobgp.enabled=true` — all pods Running
23. Verify same pod/container/RBAC checks as items 8-10
24. `kubectl get svc -n purelb-system` — verify k8gobgp-metrics Service exists (only in Helm path, gated on Prometheus.lbnodeagent.Metrics.enabled)
25. Verify BGPConfiguration CRD is installed: `kubectl get crd bgpconfigurations.bgp.purelb.io`
26. Apply a BGPConfiguration CR, deploy test LoadBalancer Service — confirm VIP route advertisement
27. **Helm upgrade test**: `helm upgrade purelb ./build/build/purelb --set gobgp.enabled=false -n purelb-system` — verify k8gobgp sidecar is removed, RBAC cleaned up, lbnodeagent pod restarts with single container
28. **Helm re-enable**: `helm upgrade purelb ./build/build/purelb --set gobgp.enabled=true -n purelb-system` — verify sidecar returns, RBAC recreated
29. Clean up: `helm uninstall purelb -n purelb-system`

### On test cluster — Base install regression (local-kvm or prox-purelb2)
30. `make install-manifest` then `kubectl apply -f deployments/install-${SUFFIX}.yaml` — verify lbnodeagent pods have **only 1 container** (no k8gobgp), no k8gobgp RBAC exists, no BGPConfiguration CRD installed
31. `helm install purelb ./build/build/purelb -n purelb-system` (without `--set gobgp.enabled=true`) — verify same: single container, no k8gobgp resources

## Notes

- k8gobgp pinned to `v0.2.2` (latest as of 2026-03-30). To upgrade: bump `GOBGP_TAG`, run `make fetch-gobgp-crd`, re-run `make helm` and manifest targets.
- The BGPConfiguration CRD is bundled in both kustomize and Helm — operators do NOT need a separate k8gobgp install.
- k8gobgp runs in `purelb-system` (not `k8gobgp-system`). This is intentional for the sidecar model.
- **Metrics pattern**: Kustomize uses pod annotation-based Prometheus discovery (`prometheus.io/port: '7472'`) — no Service needed and none is added. The k8gobgp port 7473 is only exposed via a Service in Helm, gated on `Prometheus.lbnodeagent.Metrics.enabled`, matching the existing `service-metrics-lbnodeagent.yaml` pattern.
- **Port consistency**: lbnodeagent=7472, k8gobgp-metrics=7473, k8gobgp-health=7474. All PureLB-related ports in a consistent range. Port 179 is BGP standard (reserved on host).
