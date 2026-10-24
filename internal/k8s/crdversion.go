// Copyright 2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/logging"
)

// pureLBCRDs are the CRDs PureLB installs; packaging stamps each with the
// release it belongs to (hack/stamp-crd-version.py).
var pureLBCRDs = []string{
	"servicegroups.purelb.io",
	"lbnodeagents.purelb.io",
	"egresses.purelb.io",
	"clusteregresses.purelb.io",
	"egressendpoints.purelb.io",
	"egressaddresses.purelb.io",
	"egressmembers.purelb.io",
	"egressnodestates.purelb.io",
}

const (
	crdVersionAnnotation = "purelb.io/crd-version"
	crdCheckInterval     = 10 * time.Minute
)

// watchCRDVersions compares each installed PureLB CRD's release stamp with
// release, this binary's, now and every crdCheckInterval until stop
// closes. CRDs are applied separately from the software (and Helm never
// upgrades them), so a cluster can run new binaries against old CRDs,
// which then reject or drop fields the binaries write. A mismatch is
// reported, never fatal: purelb_crd_stale{crd} is 1, and turning stale
// logs at info and raises a Warning event on the CRD. A CRD that isn't
// installed (the egress CRDs without egress) is skipped. A build without
// a release can't tell, so it doesn't check.
func (c *Client) watchCRDVersions(stop <-chan struct{}, release string) {
	if release == "" {
		logging.Debug(c.logger, "op", "crdVersionCheck", "msg", "no release in this build, not checking CRD versions")
		return
	}
	stale := map[string]bool{}
	tick := time.NewTicker(crdCheckInterval)
	defer tick.Stop()
	for {
		c.checkCRDVersions(release, stale)
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// checkCRDVersions runs one comparison. stale holds each CRD's result
// from the last one, so only a change to stale is logged and evented.
func (c *Client) checkCRDVersions(release string, stale map[string]bool) {
	for _, name := range pureLBCRDs {
		got, found, err := c.crdVersion(name)
		if err != nil {
			logging.Info(c.logger, "op", "crdVersionCheck", "crd", name, "error", err)
			continue
		}
		if !found {
			crdStale.DeleteLabelValues(name)
			delete(stale, name)
			continue
		}
		isStale := got != release
		if isStale {
			crdStale.WithLabelValues(name).Set(1)
		} else {
			crdStale.WithLabelValues(name).Set(0)
		}
		if isStale && !stale[name] {
			logging.Info(c.logger, "op", "crdVersionCheck", "crd", name, "crdVersion", got, "release", release,
				"msg", "CRD is from another release; apply this release's CRDs")
			c.events.Eventf(&corev1.ObjectReference{Kind: "CustomResourceDefinition", APIVersion: "apiextensions.k8s.io/v1", Name: name},
				corev1.EventTypeWarning, "CRDVersionMismatch",
				"CRD %s is from release %q but PureLB is %q: apply the CRDs of %s", name, got, release, release)
		}
		stale[name] = isStale
	}
}

// crdVersion returns the release stamp on CRD name ("" when unstamped)
// and whether the CRD is installed.
func (c *Client) crdVersion(name string) (string, bool, error) {
	ctx, cancel := c.apiContext()
	defer cancel()
	res := c.client.Discovery().RESTClient().Get().
		AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions", name).Do(ctx)
	if err := res.Error(); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	raw, err := res.Raw()
	if err != nil {
		return "", false, err
	}
	var crd metav1.PartialObjectMetadata
	if err := json.Unmarshal(raw, &crd); err != nil {
		return "", false, err
	}
	return crd.Annotations[crdVersionAnnotation], true, nil
}
