# Builder for the address guard eBPF object (internal/addrguard).
#
# bpf2go is a Go program that shells out to clang, so the image needs both.
# Everything is pinned -- the base image by digest, the toolchain packages by
# exact version -- because the generated .o is committed and CI regenerates
# it and fails on any byte difference. A different clang build produces a
# different object even from identical source.
FROM golang:1.26.8-trixie@sha256:eae2aaa6add2936cbf350dd0d2628b363461542f0c4b3c0b558957e0f2997379

# The live mirror keeps only the current version of each package, so exact
# pins against it break at the next point or security release. A dated
# snapshot of the archive keeps these versions installable.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && echo 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261003T000000Z trixie main' \
      > /etc/apt/sources.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      clang-19=1:19.1.7-3+b1 \
      llvm-19=1:19.1.7-3+b1 \
      libbpf-dev=1:1.5.0-3 \
 && rm -rf /var/lib/apt/lists/*

ENV BPF2GO_CC=clang-19 \
    BPF2GO_STRIP=llvm-strip-19

# A fixed workdir: clang embeds source paths in the object's BTF line info,
# so building from a different directory changes the bytes.
WORKDIR /src
