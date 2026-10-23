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
"""Stamp PureLB's generated CRDs with the release they belong to.

usage: stamp-crd-version.py <version> <crd.yaml>...

Adds the annotation purelb.io/crd-version: <version> next to
controller-gen's own annotation, in place, editing text rather than
re-serializing so the file stays as controller-gen wrote it. The allocator,
the node agents and the Helm chart compare it with their own release to
tell that the cluster's CRDs are older than the software (CRDs are applied
separately, and Helm never upgrades them).
"""
import re
import sys

KEY = "purelb.io/crd-version"

version = sys.argv[1]
for path in sys.argv[2:]:
    text = open(path).read()
    text = re.sub(rf"\n    {re.escape(KEY)}: .*", "", text)
    stamped, n = re.subn(r"(\n    controller-gen\.kubebuilder\.io/version: [^\n]*)",
                         rf'\1\n    {KEY}: "{version}"', text, count=1)
    if n != 1:
        sys.exit(f"{path}: no controller-gen annotation to stamp next to")
    open(path, "w").write(stamped)
