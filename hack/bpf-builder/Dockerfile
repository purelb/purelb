# Builder for the eBPF objects (internal/addrguard, internal/egress/datapath).
#
# CI publishes this image when this directory changes (.github/workflows/
# bpf-builder.yml), and every other build pulls the image hack/bpf-builder.ref
# names, so only publishing runs apt. See BUILDING.md.
#
# bpf2go is a Go program that shells out to clang, so the image needs both.
# Everything is pinned -- the base image by digest, the toolchain packages by
# exact version -- because the generated .o is committed and CI regenerates
# it and fails on any byte difference. A different clang build produces a
# different object even from identical source.
FROM golang:1.26.8-trixie@sha256:eae2aaa6add2936cbf350dd0d2628b363461542f0c4b3c0b558957e0f2997379

# The live mirror keeps only the current version of each package, so a pin
# stops installing when Debian replaces it, which fails publishing and nothing
# else. hack/bpf-builder.sh update moves the pins to what the mirror has.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && echo 'deb https://cdn-aws.deb.debian.org/debian trixie main' \
      > /etc/apt/sources.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      clang-22=1:22.1.8-1~deb13u4 \
      llvm-22=1:22.1.8-1~deb13u4 \
      libbpf-dev=1:1.5.0-3 \
 && rm -rf /var/lib/apt/lists/*

ENV BPF2GO_CC=clang-22 \
    BPF2GO_STRIP=llvm-strip-22

# A fixed workdir: clang embeds source paths in the object's BTF line info,
# so building from a different directory changes the bytes.
WORKDIR /src
