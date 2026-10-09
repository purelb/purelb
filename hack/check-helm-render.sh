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
# Render the chart's address guard pieces and check them. Nothing else
# tests the Helm path (the e2e suite writes LBNodeAgents directly), so a
# values or template mistake here -- the guard switched on by an upgrade,
# an alert group that never renders -- would otherwise ship unnoticed.
#
# Usage: HELM="<helm command>" hack/check-helm-render.sh
set -euo pipefail

HELM=${HELM:-helm}
export KUBECONFIG=/dev/null  # helm template needs no cluster
CHART=build/helm/purelb
fail=0

render() { # template, extra helm args...
	local tpl=$1; shift
	$HELM template t "$CHART" --api-versions monitoring.coreos.com/v1 -s "templates/$tpl" "$@"
}
expect() { # description, pattern, rendered text
	if grep -qE -- "$2" <<<"$3"; then echo "ok   $1"; else echo "FAIL $1: no match for /$2/" >&2; fail=1; fi
}
refuse() { # description, pattern, rendered text
	if grep -qE -- "$2" <<<"$3"; then echo "FAIL $1: unexpected /$2/" >&2; fail=1; else echo "ok   $1"; fi
}

out=$(render lbnodeagent.yaml)
refuse "LBNodeAgent: no addressGuard by default (an upgrade must never turn it on)" "addressGuard" "$out"

out=$(render lbnodeagent.yaml --set lbnodeagent.addressGuard.mode=monitor --set 'lbnodeagent.addressGuard.allowedProtocols={47}')
expect "LBNodeAgent: addressGuard rendered when set" "^    addressGuard:" "$out"
expect "LBNodeAgent: its fields are carried" "mode: monitor" "$out"

out=$(render lbnodeagent.yaml --set-json 'lbnodeagent.addressGuard={}')
expect "LBNodeAgent: addressGuard: {} (all defaults) enables the guard" "^    addressGuard:" "$out"

out=$(render lbnodeagent.yaml --set-json 'lbnodeagent.addressGuard=null')
refuse "LBNodeAgent: addressGuard: null leaves it off" "addressGuard" "$out"

out=$(render daemonset.yaml)
expect "DaemonSet: lbnodeagent has the BPF capability" "^ *- BPF$" "$out"

rules=(--set Prometheus.lbnodeagent.prometheusRules.enabled=true)
out=$(render prometheusrules-lbnodeagent.yaml "${rules[@]}")
refuse "PrometheusRule: no guard alerts by default" "PurelbAddressGuard" "$out"

out=$(render prometheusrules-lbnodeagent.yaml "${rules[@]}" --set Prometheus.lbnodeagent.prometheusRules.addressGuardAlerts=true)
expect "PrometheusRule: guard alerts render with no user rules" "alert: PurelbAddressGuardUnguardedVIPs" "$out"
expect "PrometheusRule: attach-error alert" "alert: PurelbAddressGuardAttachErrors" "$out"
expect "PrometheusRule: stand-down alert" "alert: PurelbAddressGuardStandingDown" "$out"
expect "PrometheusRule: restart-required alert" "alert: PurelbAddressGuardRestartRequired" "$out"
expect "PrometheusRule: failed-VIPs alert" "alert: PurelbAddressGuardFailedVIPs" "$out"

# The chart's guard rules and the manifest install's must be the same rules:
# compare them as rendered into purelb-system, from the first guard alert on.
guard_rules() { sed -n '/- alert: PurelbAddressGuardUnguardedVIPs/,$p'; }
helm_rules=$(render prometheusrules-lbnodeagent.yaml "${rules[@]}" --namespace purelb-system \
	--set Prometheus.lbnodeagent.prometheusRules.addressGuardAlerts=true | guard_rules)
if diff <(echo "$helm_rules") <(guard_rules <monitoring/prometheusrules-address-guard.yaml) >/dev/null; then
	echo "ok   PrometheusRule: Helm and monitoring/prometheusrules-address-guard.yaml rules are identical"
else
	echo "FAIL PrometheusRule: Helm and monitoring/ guard rules differ:" >&2
	diff <(echo "$helm_rules") <(guard_rules <monitoring/prometheusrules-address-guard.yaml) >&2 || true
	fail=1
fi
expect "PrometheusRule: Prometheus templating survives Helm" '\{\{ \$labels.instance \}\}' "$out"

out=$(render prometheusrules-lbnodeagent.yaml "${rules[@]}" --set Prometheus.lbnodeagent.prometheusRules.addressGuardAlerts=true \
	--set 'Prometheus.lbnodeagent.prometheusRules.rules[0].alert=UserRule' --set 'Prometheus.lbnodeagent.prometheusRules.rules[0].expr=up==0')
expect "PrometheusRule: user rules kept alongside" "alert: UserRule" "$out"
expect "PrometheusRule: guard group alongside user rules" "name: purelb-address-guard" "$out"

exit $fail
