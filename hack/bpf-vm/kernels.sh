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
# BPF_VM_MODULES=egress also stages the modules the egress spikes need
# (Geneve, nf_tables NAT and conntrack, FIB, bridge), with their
# dependencies resolved from modinfo, decompressed on the host (not every
# kernel can decompress modules itself) and numbered so /init's
# filename-order loading is dependency order. Cached separately.
#
#   debian-6.6.15                  Debian's last 6.6 (the egress and address
#                                  guard kernel floor; no netkit before 6.7)
#   debian-6.12.111                Debian 13's kernel
#   debian-6.18.12-bpo             Debian 13 backports; 6.18 before 6.18.14
#                                  has the speculation-barrier verifier bug
#   ubuntu-6.8.0-146-generic       Ubuntu 24.04's kernel
#   ubuntu-6.17.0-1022-azure       Ubuntu 24.04's 6.17 (HWE): CI's runner
#                                  kernel, with the same bug
#
# Needs curl, sha256sum, dpkg-deb, xz and zstd; with BPF_VM_MODULES, modinfo.
set -euo pipefail

CACHE=${BPF_VM_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/purelb-bpf-vm}
NAME=${1:?usage: kernels.sh <kernel name>}
SET=${BPF_VM_MODULES:-}
case $SET in "" | egress) ;; *) echo "kernels.sh: unknown BPF_VM_MODULES=$SET" >&2; exit 2 ;; esac
DIR=$CACHE/$NAME${SET:+-$SET}
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
debian-6.6.15)
	REL=6.6.15-amd64
	fetch $SNAPSHOT/d4a8175b0a1f392d821324896e957217e4074419 d8113b4cb5447c1cc46c5d44fdc67909e143949044550450e929cfe1079607ad
	;;
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
mainline-6.6.159)
	# Not in the default matrix: Ubuntu's mainline build of the latest 6.6.y,
	# for checking 6.6.15 findings against a current 6.6 stable kernel.
	REL=6.6.159-0606159-generic
	MAINLINE=https://kernel.ubuntu.com/mainline/v6.6.159/amd64
	fetch $MAINLINE/linux-image-unsigned-6.6.159-0606159-generic_6.6.159-0606159.202610151308_amd64.deb d094764fbfb698dbe47ac9560b250a419faf8bd8d842bcea5e3b673f7aa90979
	fetch $MAINLINE/linux-modules-6.6.159-0606159-generic_6.6.159-0606159.202610151308_amd64.deb 63362254684e31211e0c07325c8f3327d80162d1ed8a963ae4226de15cafe1a7
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
			# Decompressed here: not every kernel can load compressed
			# modules itself (Debian's 6.6 has no MODULE_DECOMPRESS).
			case $ko in
			*.xz) xz -dc "$ko" >"$TMP/out/modules/$m.ko" ;;
			*.zst) zstd -dcq "$ko" >"$TMP/out/modules/$m.ko" ;;
			*) cp "$ko" "$TMP/out/modules/" ;;
			esac
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

if [ "$SET" = egress ]; then
	export PATH="$PATH:/usr/sbin:/sbin"
	# Netkit is built in (=y) from 6.7; everything here is a module on
	# distribution kernels. A name missing from both the tree and
	# modules.builtin is an error, so a kernel that lacks one is not
	# silently tested without it.
	# The crc32c algorithm goes first (below): libcrc32c only soft-depends
	# on it (modinfo softdep), which modprobe would load and /init cannot.
	EGRESS_MODS="geneve nf_tables nft_ct nft_nat nft_chain_nat nft_fib_inet nf_conntrack_netlink bridge br_netfilter"
	declare -A path=() seen=()
	while IFS= read -r f; do
		b=${f##*/}
		b=${b%%.ko*}
		path[${b//-/_}]=$f
	done < <(find "$MODS/kernel" -name '*.ko*')
	order=()
	visit() {
		local m=${1//-/_} f d
		[ -n "${seen[$m]:-}" ] && return 0
		seen[$m]=1
		if grep -qE "/(${m}|${1})\.ko$" "$MODS/modules.builtin" 2>/dev/null; then
			return 0
		fi
		f=${path[$m]:-}
		if [ -z "$f" ]; then
			echo "kernels.sh: $NAME: no module $1" >&2
			exit 1
		fi
		for d in $(modinfo -F depends "$f" | tr ',' ' '); do
			visit "$d"
		done
		order+=("$f")
	}
	# The crc32c algorithm's module name varies (crc32c_generic up to 6.13;
	# later kernels build it in or rename it): stage whichever exists.
	for m in crc32c_generic crc32c_cryptoapi; do
		[ -n "${path[$m]:-}" ] && visit "$m"
	done
	for m in $EGRESS_MODS; do
		visit "$m"
	done
	i=0
	for f in "${order[@]}"; do
		i=$((i + 1))
		b=${f##*/}
		out=$TMP/out/modules/$(printf %02d $i)-${b%%.ko*}.ko
		case $f in
		*.xz) xz -dc "$f" >"$out" ;;
		*.zst) zstd -dcq "$f" >"$out" ;;
		*) cp "$f" "$out" ;;
		esac
	done
fi

mkdir -p "$CACHE"
rm -rf "$DIR"
mv "$TMP/out" "$DIR"
echo "$DIR"
