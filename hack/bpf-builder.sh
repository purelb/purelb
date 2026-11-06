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
# The builder image's version control (BUILDING.md):
#
#   tag     the image tag of the committed hack/bpf-builder: its git tree hash
#   digest  the digest published under that tag, empty if there is none
#   check   fail unless hack/bpf-builder.ref is that tag and digest, and
#           go.mod's Go version is the Dockerfile's
#   pin     write that tag and digest to hack/bpf-builder.ref
#   update  move the Dockerfile's pins, and go.mod, to the newest clang in
#           Debian trixie and the newest Go; print what changed
#
# This file stays outside hack/bpf-builder, or changing it would change the tag.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

REPO=ghcr.io/purelb/purelb/bpf-builder
REF=hack/bpf-builder.ref
DOCKERFILE=hack/bpf-builder/Dockerfile
MIRROR=https://cdn-aws.deb.debian.org/debian

die() { echo "bpf-builder: $*" >&2; exit 1; }

tag() { echo "tree-$(git rev-parse HEAD:hack/bpf-builder)"; }

# registry_digest <token URL> <registry> <repository> <tag>: empty when there
# is no such tag, or anonymous pulls of it aren't allowed.
registry_digest() {
  local token
  token=$(curl -fsSL "$1" 2>/dev/null | sed -E 's/.*"token":"([^"]+)".*/\1/') || return 0
  [[ -n $token ]] || return 0
  curl -sSI -H "Authorization: Bearer $token" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://$2/v2/$3/manifests/$4" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}'
}

digest() {
  registry_digest "https://ghcr.io/token?scope=repository:${REPO#ghcr.io/}:pull" ghcr.io "${REPO#ghcr.io/}" "$(tag)"
}

check() {
  local want have d go_mod go_image
  go_mod=$(awk '$1=="go"{print $2}' go.mod)
  go_image=$(sed -nE 's/^FROM golang:([0-9.]+)-.*/\1/p' "$DOCKERFILE")
  [[ $go_mod == "$go_image" ]] ||
    die "go.mod says Go $go_mod and $DOCKERFILE says $go_image: change them together (make bpf runs the image's Go)"
  want="$REPO:$(tag)"
  have=$(cat "$REF")
  [[ $have =~ ^"$want"@sha256:[0-9a-f]{64}$ ]] ||
    die "$REF is '$have', and hack/bpf-builder needs $want. Wait for the 'BPF builder image' workflow on this PR, then run make bpf-builder-pin and commit $REF."
  d=$(digest)
  [[ -n $d ]] || die "$want isn't on the registry, or isn't public"
  [[ $have == "$want@$d" ]] || die "$REF has digest ${have#*@}, and the registry has $d under that tag"
  echo "bpf-builder: $have"
}

pin() {
  local d
  git diff --quiet HEAD -- hack/bpf-builder || die "commit hack/bpf-builder first: the tag is the committed tree's hash"
  d=$(digest)
  [[ -n $d ]] || die "$REPO:$(tag) isn't on the registry, or isn't public: has the 'BPF builder image' workflow finished on the PR?"
  echo "$REPO:$(tag)@$d" > "$REF"
  echo "bpf-builder: $REF is now $(cat "$REF"); commit it"
}

update() {
  local pkgs n clang llvm libbpf go_new go_old base
  pkgs=$(curl -fsSL "$MIRROR/dists/trixie/main/binary-amd64/Packages.xz" | xz -dc |
    awk '/^Package:/{p=$2} /^Version:/{print p, $2}')
  n=$(awk '$1 ~ /^clang-[0-9]+$/{sub("clang-","",$1); print $1}' <<<"$pkgs" | sort -n | tail -1)
  clang=$(awk -v p="clang-$n" '$1==p{print $2}' <<<"$pkgs")
  llvm=$(awk -v p="llvm-$n" '$1==p{print $2}' <<<"$pkgs")
  libbpf=$(awk '$1=="libbpf-dev"{print $2}' <<<"$pkgs")
  [[ -n $n && -n $clang && -n $llvm && -n $libbpf ]] || die "couldn't read the versions from $MIRROR"
  sed -i -E "s#clang-[0-9]+=[^ ]+#clang-$n=$clang#; s#llvm-[0-9]+=[^ ]+#llvm-$n=$llvm#; s#libbpf-dev=[^ ]+#libbpf-dev=$libbpf#; s#BPF2GO_CC=clang-[0-9]+#BPF2GO_CC=clang-$n#; s#llvm-strip-[0-9]+#llvm-strip-$n#" "$DOCKERFILE"

  go_old=$(awk '$1=="go"{print $2}' go.mod)
  go_new=$(curl -fsSL 'https://go.dev/dl/?mode=json' | grep -oE '"version": *"go[0-9.]+"' | head -1 | grep -oE '[0-9]+(\.[0-9]+)+')
  [[ -n $go_new ]] || die "couldn't read the newest Go from go.dev"
  if [[ $go_new != "$go_old" ]]; then
    base=$(registry_digest "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/golang:pull" \
      registry-1.docker.io library/golang "$go_new-trixie")
    [[ -n $base ]] || die "no golang:$go_new-trixie image yet"
    sed -i -E "s#^FROM golang:.*#FROM golang:$go_new-trixie@$base#" "$DOCKERFILE"
    go mod edit -go="$go_new"
    GOTOOLCHAIN=auto go mod tidy
  fi
  git diff --stat -- "$DOCKERFILE" go.mod go.sum
}

case "${1:-}" in
  tag|digest|check|pin|update) "$1" ;;
  *) die "usage: $0 tag|digest|check|pin|update" ;;
esac
