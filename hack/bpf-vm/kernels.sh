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
# Fetch a kernel the address guard's BPF code is tested on, and print the
# directory holding it: <dir>/vmlinuz, and <dir>/modules/*.ko* to load at
# boot -- the link types TestAttacherInNetns creates (veth, dummy, tun;
# nlmon where the package has it), unless built in. Distribution kernels,
# as packaged, from URLs that stay put, pinned by checksum. Cached under
# $BPF_VM_CACHE (default ~/.cache/purelb-bpf-vm).
#
#   debian-6.12.111                Debian 13's kernel
#   debian-6.18.12-bpo             Debian 13 backports; 6.18 before 6.18.14
#                                  has the speculation-barrier verifier bug
#   ubuntu-6.8.0-146-generic       Ubuntu 24.04's kernel
#   ubuntu-6.17.0-1022-azure       Ubuntu 24.04's 6.17 (HWE): CI's runner
#                                  kernel, with the same bug
#
# Needs curl, sha256sum and dpkg-deb.
set -euo pipefail

CACHE=${BPF_VM_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/purelb-bpf-vm}
NAME=${1:?usage: kernels.sh <kernel name>}
DIR=$CACHE/$NAME
if [ -s "$DIR/vmlinuz" ]; then
	echo "$DIR"
	exit 0
fi
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/out/modules" "$TMP/x"

# fetch <url> <sha256>: download a package and unpack it into $TMP/x.
fetch() {
	curl -fsSL --retry 3 -o "$TMP/pkg.deb" "$1"
	echo "$2  $TMP/pkg.deb" | sha256sum -c --quiet
	dpkg-deb -x "$TMP/pkg.deb" "$TMP/x"
}

SNAPSHOT=https://snapshot.debian.org/file
LAUNCHPAD=https://launchpad.net/ubuntu/+archive/primary/+files
case $NAME in
debian-6.12.111)
	REL=6.12.111+deb13-amd64
	# linux-image-6.12.111+deb13-amd64-unsigned_6.12.111-1_amd64.deb
	fetch $SNAPSHOT/e8dccaf61123e3be2b9a86345ebe26f55b120207 0dd8541215c133f7df9e80a49c0b6e89e8680c4de364fc41bfb3a9ca81a6e4e4
	;;
debian-6.18.12-bpo)
	REL=6.18.12+deb13-amd64
	# linux-image-6.18.12+deb13-amd64-unsigned_6.18.12-1~bpo13+1_amd64.deb
	fetch $SNAPSHOT/e16e7332b6c1b45f5ec297c671f82bbc85da867e 57e79429ca2e2c474f7bb4864f418317b3639148075c4794bc0a123048077d44
	;;
ubuntu-6.8.0-146-generic)
	REL=6.8.0-146-generic
	fetch $LAUNCHPAD/linux-image-unsigned-6.8.0-146-generic_6.8.0-146.146_amd64.deb 01f6ed468ea6fae86dca107d89fc345cd69c658bb1daf97c35c21984ff6dcc7b
	fetch $LAUNCHPAD/linux-modules-6.8.0-146-generic_6.8.0-146.146_amd64.deb 9287a0b722a7dd0c94a412eda8e27de4742a7925fdf2d08375b2f608516e6ab4
	;;
ubuntu-6.17.0-1022-azure)
	REL=6.17.0-1022-azure
	fetch $LAUNCHPAD/linux-image-unsigned-6.17.0-1022-azure_6.17.0-1022.22_amd64.deb d36fad892a6ab594527866d5047080a7b77382f0e89a9a9cab00a125bcfb82a6
	fetch $LAUNCHPAD/linux-modules-6.17.0-1022-azure_6.17.0-1022.22_amd64.deb 499f3c4e76a19b51e399aa9dede097eabc0ca001471c27b34f5a1d1d54b7c44c
	;;
*)
	echo "kernels.sh: unknown kernel $NAME" >&2
	exit 2
	;;
esac

cp "$TMP/x/boot/vmlinuz-$REL" "$TMP/out/vmlinuz"
CONFIG=$TMP/x/boot/config-$REL
MODS=$TMP/x/lib/modules/$REL # Debian: under /usr
[ -d "$MODS" ] || MODS=$TMP/x/usr/lib/modules/$REL
# None of these modules depends on another. The kernels decompress
# modules themselves (MODULE_DECOMPRESS), so they are copied as packaged.
for m in veth dummy tun nlmon; do
	case $(grep -E "^CONFIG_${m^^}=" "$CONFIG" || true) in
	*=y) ;;
	*=m)
		ko=$(ls "$MODS/kernel/drivers/net/$m".ko* 2>/dev/null || true)
		if [ -n "$ko" ]; then
			cp "$ko" "$TMP/out/modules/"
		elif [ $m != nlmon ]; then # Ubuntu ships nlmon in modules-extra
			echo "kernels.sh: $NAME: no $m module" >&2
			exit 1
		fi
		;;
	*)
		if [ $m != nlmon ]; then
			echo "kernels.sh: $NAME: kernel has no $m" >&2
			exit 1
		fi
		;;
	esac
done

mkdir -p "$CACHE"
rm -rf "$DIR"
mv "$TMP/out" "$DIR"
echo "$DIR"
