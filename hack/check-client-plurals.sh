#!/usr/bin/env bash
# Copyright 2026 Acnodal Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Fail if a generated typed client requests a resource name its CRD
# doesn't serve. client-gen and controller-gen pluralize independently:
# for EgressEndpoints client-gen chose "egressendpointses" while the CRD
# serves "egressendpoints", so every request would have been a 404. A
# kind whose names differ needs +resourceName on its type.
#
# Usage: hack/check-client-plurals.sh (after `make generate crd`)
set -euo pipefail

fail=0
for crd in deployments/crds/purelb.io_*.yaml; do
	read -r kind plural < <(python3 -c '
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
print(d["spec"]["names"]["kind"], d["spec"]["names"]["plural"])' "$crd")
	client=pkg/generated/clientset/versioned/typed/purelb/v2/$(tr '[:upper:]' '[:lower:]' <<<"$kind").go
	# The resource name is the first string argument of gentype.NewClient*.
	got=$(awk '/gentype\.NewClient/{f=1} f && /"[a-z]+",/{gsub(/[^a-z]/,""); print; exit}' "$client")
	if [ "$got" != "$plural" ]; then
		echo "FAIL $kind: client requests \"$got\", the CRD serves \"$plural\" (add +resourceName=$plural)" >&2
		fail=1
	else
		echo "ok   $kind: $plural"
	fi
done
exit $fail
