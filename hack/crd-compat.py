#!/usr/bin/env python3
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
"""Compare two directories of CRDs for changes that break existing objects.

usage: crd-compat.py <old-dir> <new-dir>

A change breaks when an object or manifest valid under the old CRD can be
rejected or lose data under the new one: a CRD, version or field removed,
a type or scope changed, a field newly required, an enum narrowed, a limit
tightened, a pattern added or changed. Additions pass. New or changed CEL
rules are reported but don't fail: the API server ratchets them, so
unchanged fields of existing objects still validate.

Exit status 1 if anything breaks.
"""
import glob
import os
import sys

import yaml

breaks, notes = [], []

# limit keyword -> True if a larger value is looser
LIMITS = {
    "maximum": True, "maxLength": True, "maxItems": True, "maxProperties": True,
    "minimum": False, "minLength": False, "minItems": False, "minProperties": False,
}


def load(d):
    out = {}
    for f in sorted(glob.glob(os.path.join(d, "*.yaml"))):
        for doc in yaml.safe_load_all(open(f)):
            if doc and doc.get("kind") == "CustomResourceDefinition":
                out[doc["metadata"]["name"]] = doc
    return out


def walk(old, new, path):
    if old.get("type") != new.get("type"):
        breaks.append(f"{path}: type {old.get('type')} -> {new.get('type')}")
        return
    if old.get("x-kubernetes-preserve-unknown-fields") and not new.get("x-kubernetes-preserve-unknown-fields"):
        breaks.append(f"{path}: no longer preserves unknown fields")

    for key, looser_up in LIMITS.items():
        o, n = old.get(key), new.get(key)
        if n is None:
            continue
        if o is None or (n < o if looser_up else n > o):
            breaks.append(f"{path}: {key} {o} -> {n}")

    if "enum" in new:
        if "enum" not in old:
            breaks.append(f"{path}: enum added {new['enum']}")
        else:
            gone = [v for v in old["enum"] if v not in new["enum"]]
            if gone:
                breaks.append(f"{path}: enum values removed {gone}")
    if new.get("pattern") is not None and new.get("pattern") != old.get("pattern"):
        breaks.append(f"{path}: pattern {old.get('pattern')!r} -> {new.get('pattern')!r}")

    for req in sorted(set(new.get("required", [])) - set(old.get("required", []))):
        breaks.append(f"{path}.{req}: newly required")

    if old.get("default") != new.get("default") and "default" in old:
        notes.append(f"{path}: default {old.get('default')!r} -> {new.get('default')!r}")

    orules = {r["rule"] for r in old.get("x-kubernetes-validations", [])}
    for r in new.get("x-kubernetes-validations", []):
        if r["rule"] not in orules:
            notes.append(f"{path}: new CEL rule: {r['rule']}")

    oprops, nprops = old.get("properties", {}), new.get("properties", {})
    for name, osch in oprops.items():
        if name not in nprops:
            breaks.append(f"{path}.{name}: removed")
        else:
            walk(osch, nprops[name], f"{path}.{name}")
    if isinstance(old.get("items"), dict) and isinstance(new.get("items"), dict):
        walk(old["items"], new["items"], f"{path}[]")
    if isinstance(old.get("additionalProperties"), dict) and isinstance(new.get("additionalProperties"), dict):
        walk(old["additionalProperties"], new["additionalProperties"], f"{path}{{}}")


def main():
    old, new = load(sys.argv[1]), load(sys.argv[2])
    for name, ocrd in old.items():
        ncrd = new.get(name)
        if ncrd is None:
            breaks.append(f"{name}: CRD removed")
            continue
        if ocrd["spec"]["scope"] != ncrd["spec"]["scope"]:
            breaks.append(f"{name}: scope {ocrd['spec']['scope']} -> {ncrd['spec']['scope']}")
        nvers = {v["name"]: v for v in ncrd["spec"]["versions"]}
        for ov in ocrd["spec"]["versions"]:
            nv = nvers.get(ov["name"])
            if nv is None or (ov.get("served") and not nv.get("served")):
                breaks.append(f"{name}/{ov['name']}: version removed or no longer served")
                continue
            if "status" in ov.get("subresources", {}) and "status" not in nv.get("subresources", {}):
                breaks.append(f"{name}/{ov['name']}: status subresource removed")
            walk(ov["schema"]["openAPIV3Schema"], nv["schema"]["openAPIV3Schema"], f"{name}/{ov['name']}")
    for name in sorted(set(new) - set(old)):
        notes.append(f"{name}: new CRD")

    for n in notes:
        print(f"note   {n}")
    for b in breaks:
        print(f"BREAK  {b}")
    print(f"{len(old)} CRDs before, {len(new)} after: {len(breaks)} breaking change(s), {len(notes)} note(s)")
    sys.exit(1 if breaks else 0)


if __name__ == "__main__":
    main()
