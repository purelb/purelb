// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2026 Acnodal Inc.
//
// Address guard: drop packets addressed to a PureLB VIP unless their
// (protocol, port) is one of that VIP's Service ports.
//
// One parser, two entry points: tcx ingress (the default) and native XDP
// (opt-in). Packets not addressed to a guarded address take the fast path:
// one header read and one map lookup, uncounted.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define ETH_P_8021Q_BE  bpf_htons(0x8100)
#define ETH_P_8021AD_BE bpf_htons(0x88A8)
#define ETH_P_IP_BE     bpf_htons(0x0800)
#define ETH_P_IPV6_BE   bpf_htons(0x86DD)

#define MAX_VLAN_TAGS   2
#define MAX_EXT_HDRS    8

#define IPPROTO_HOPOPTS_  0
#define IPPROTO_ROUTING_  43
#define IPPROTO_FRAGMENT_ 44
#define IPPROTO_ICMPV6_   58
#define IPPROTO_DSTOPTS_  60

/* Index layout of ag_reasons: (action * REASON_MAX + reason) * 2 + family. */
enum action { ACT_PASS = 0, ACT_DROP = 1, ACT_WOULD_DROP = 2, ACT_MAX = 3 };
enum reason {
	R_PORT_ALLOWED = 0,
	R_PROTO_ALLOWED,
	R_ICMP,
	R_FRAGMENT,
	R_PORT_DENIED,
	R_PROTO_DENIED,
	R_ICMP_DENIED,
	R_MALFORMED,
	REASON_MAX,
};
enum family { FAM_V4 = 0, FAM_V6 = 1 };
enum mode { MODE_ENFORCE = 0, MODE_MONITOR = 1 };

struct vip_counters {
	__u64 drop;
	__u64 would_drop;
};

/* Allowed (address, protocol, port). An IPv4 address occupies addr[0]; the
 * rest stays zero, and family keeps the two families from ever aliasing. */
struct port_key {
	__u32 addr[4];
	__be16 port;
	__u8 proto;
	__u8 family;
};

struct guard_config {
	__u32 mode;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 4096);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, __be32);
	__type(value, struct vip_counters);
} ag_vip4 SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 4096);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct in6_addr);
	__type(value, struct vip_counters);
} ag_vip6 SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct port_key);
	__type(value, __u8);
} ag_ports SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 256);
	__type(key, __u32);
	__type(value, __u8);
} ag_protos SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct guard_config);
} ag_config SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, ACT_MAX * REASON_MAX * 2);
	__type(key, __u32);
	__type(value, __u64);
} ag_reasons SEC(".maps");

/* Verdict returned by the shared parser. */
enum verdict { V_PASS = 0, V_DROP = 1 };

static __always_inline int load(void *ctx, int xdp, __u32 off, void *to, __u32 len)
{
	struct __sk_buff *skb = ctx;
	__u32 want;

	if (xdp)
		return bpf_xdp_load_bytes(ctx, off, to, len);
	/* Relative to the network header, so L3 devices (no Ethernet header)
	 * read the same way as L2 ones. */
	if (bpf_skb_load_bytes_relative(ctx, off, to, len, BPF_HDR_START_NET) == 0)
		return 0;
	/* bpf_skb_load_bytes_relative reads only the skb's linear area, and
	 * nothing pulls a non-TCP/UDP transport header there before tc runs:
	 * a 1280-byte ICMPv6 Packet Too Big can arrive with its type byte in
	 * a page fragment. Reading it as malformed dropped the PMTUD messages
	 * the guard exists to pass. Pull enough to cover the read -- counted
	 * from skb->data, which at tc ingress is the MAC header -- and try
	 * again. Only a packet whose headers aren't already linear gets here. */
	want = off + len + ETH_HLEN;
	if (want > skb->len)
		want = skb->len;
	if (bpf_skb_pull_data(skb, want) < 0)
		return -1;
	return bpf_skb_load_bytes_relative(ctx, off, to, len, BPF_HDR_START_NET);
}

static __always_inline void count(__u32 action, __u32 reason, __u32 fam)
{
	__u32 idx = (action * REASON_MAX + reason) * 2 + fam;
	__u64 *c = bpf_map_lookup_elem(&ag_reasons, &idx);

	if (c)
		*c += 1; /* per-CPU: no atomics needed */
}

static __always_inline int monitoring(void)
{
	__u32 zero = 0;
	struct guard_config *cfg = bpf_map_lookup_elem(&ag_config, &zero);

	return cfg && cfg->mode == MODE_MONITOR;
}

/* Record the outcome for a guarded address and turn it into a verdict. */
static __always_inline int decide(struct vip_counters *vc, int pass, __u32 reason, __u32 fam)
{
	if (pass) {
		count(ACT_PASS, reason, fam);
		return V_PASS;
	}
	if (monitoring()) {
		count(ACT_WOULD_DROP, reason, fam);
		vc->would_drop += 1;
		return V_PASS;
	}
	count(ACT_DROP, reason, fam);
	vc->drop += 1;
	return V_DROP;
}

static __always_inline int icmp_allowed(__u8 type, __u32 fam)
{
	if (fam == FAM_V4)
		/* echo reply, dest unreachable (incl. frag-needed: PMTUD),
		 * echo request, time exceeded, parameter problem */
		return type == 0 || type == 3 || type == 8 || type == 11 || type == 12;
	/* dest unreachable, packet too big (PMTUD), time exceeded, parameter
	 * problem, echo request/reply, neighbor solicitation/advertisement
	 * (unicast NUD to the VIP) */
	return (type >= 1 && type <= 4) || type == 128 || type == 129 ||
	       type == 135 || type == 136;
}

/* Extension headers walked to find the transport header. The fragment
 * header (44) is one too; AH (51) is deliberately not -- it is treated as
 * a plain protocol in both families. */
static __always_inline int is_ext_hdr(__u8 p)
{
	return p == IPPROTO_HOPOPTS_ || p == IPPROTO_ROUTING_ ||
	       p == IPPROTO_DSTOPTS_ || p == IPPROTO_FRAGMENT_;
}

/* proto is the L3 ethertype (network order) and off the offset of the
 * header it describes, both as the hook presents them. */
static __always_inline int guard(void *ctx, int xdp, __be16 proto, __u32 off)
{
	struct vip_counters *vc;
	struct port_key pk;
	__u32 fam, l4off;
	__u8 l4proto;

	/* Skip in-band VLAN tags: each is [TCI][encapsulated ethertype]. With
	 * tcx the kernel has already moved the outer tag to metadata, so only
	 * an inner (QinQ) tag is still here. */
#pragma unroll
	for (int i = 0; i < MAX_VLAN_TAGS; i++) {
		if (proto != ETH_P_8021Q_BE && proto != ETH_P_8021AD_BE)
			break;
		__be16 tag[2];

		if (load(ctx, xdp, off, tag, sizeof(tag)) < 0)
			return V_PASS;
		proto = tag[1];
		off += 4;
	}

	if (proto == ETH_P_IP_BE) {
		struct iphdr ip;

		if (load(ctx, xdp, off, &ip, sizeof(ip)) < 0)
			return V_PASS;
		vc = bpf_map_lookup_elem(&ag_vip4, &ip.daddr);
		if (!vc)
			return V_PASS;
		fam = FAM_V4;
		if (ip.version != 4 || ip.ihl < 5)
			return decide(vc, 0, R_MALFORMED, fam);
		if (bpf_ntohs(ip.frag_off) & 0x1fff)
			return decide(vc, 1, R_FRAGMENT, fam);
		l4off = off + ip.ihl * 4;
		l4proto = ip.protocol;
		__builtin_memset(&pk, 0, sizeof(pk));
		pk.addr[0] = ip.daddr;
	} else if (proto == ETH_P_IPV6_BE) {
		struct ipv6hdr ip6;

		if (load(ctx, xdp, off, &ip6, sizeof(ip6)) < 0)
			return V_PASS;
		vc = bpf_map_lookup_elem(&ag_vip6, &ip6.daddr);
		if (!vc)
			return V_PASS;
		fam = FAM_V6;
		l4off = off + sizeof(ip6);
		l4proto = ip6.nexthdr;

#pragma unroll
		for (int i = 0; i < MAX_EXT_HDRS; i++) {
			if (l4proto == IPPROTO_FRAGMENT_) {
				__u8 h[4]; /* next header, reserved, offset+flags */

				if (load(ctx, xdp, l4off, h, sizeof(h)) < 0)
					return decide(vc, 0, R_MALFORMED, fam);
				if ((((__u16)h[2] << 8) | h[3]) & 0xfff8)
					return decide(vc, 1, R_FRAGMENT, fam);
				l4proto = h[0];
				l4off += 8;
			} else if (is_ext_hdr(l4proto)) {
				__u8 h[2]; /* next header, length in 8-octet units - 1 */

				if (load(ctx, xdp, l4off, h, sizeof(h)) < 0)
					return decide(vc, 0, R_MALFORMED, fam);
				l4proto = h[0];
				l4off += ((__u32)h[1] + 1) * 8;
			} else {
				break;
			}
		}
		/* Still looking at an extension header after walking the
		 * maximum: the chain is longer than we parse. */
		if (is_ext_hdr(l4proto))
			return decide(vc, 0, R_MALFORMED, fam);
		__builtin_memset(&pk, 0, sizeof(pk));
		__builtin_memcpy(pk.addr, &ip6.daddr, sizeof(pk.addr));
	} else {
		/* Not IP, or more VLAN tags than we skip. */
		return V_PASS;
	}

	if ((fam == FAM_V4 && l4proto == IPPROTO_ICMP) ||
	    (fam == FAM_V6 && l4proto == IPPROTO_ICMPV6_)) {
		__u8 type;

		if (load(ctx, xdp, l4off, &type, sizeof(type)) < 0)
			return decide(vc, 0, R_MALFORMED, fam);
		if (icmp_allowed(type, fam))
			return decide(vc, 1, R_ICMP, fam);
		return decide(vc, 0, R_ICMP_DENIED, fam);
	}

	if (l4proto == IPPROTO_TCP || l4proto == IPPROTO_UDP || l4proto == IPPROTO_SCTP) {
		__be16 ports[2]; /* source, destination: same layout in all three */

		/* A first fragment too short to carry the ports cannot be
		 * checked, and the rest of the datagram would pass as non-first
		 * fragments: drop it. */
		if (load(ctx, xdp, l4off, ports, sizeof(ports)) < 0)
			return decide(vc, 0, R_MALFORMED, fam);
		pk.port = ports[1];
		pk.proto = l4proto;
		pk.family = fam;
		if (bpf_map_lookup_elem(&ag_ports, &pk))
			return decide(vc, 1, R_PORT_ALLOWED, fam);
		return decide(vc, 0, R_PORT_DENIED, fam);
	}

	__u32 p = l4proto;
	__u8 *allowed = bpf_map_lookup_elem(&ag_protos, &p);

	if (allowed && *allowed)
		return decide(vc, 1, R_PROTO_ALLOWED, fam);
	return decide(vc, 0, R_PROTO_DENIED, fam);
}

SEC("tcx/ingress")
int ag_tcx(struct __sk_buff *skb)
{
	if (guard(skb, 0, skb->protocol, 0) == V_DROP)
		return TCX_DROP;
	return TCX_NEXT;
}

SEC("xdp.frags")
int ag_xdp(struct xdp_md *ctx)
{
	__be16 proto;

	if (bpf_xdp_load_bytes(ctx, 12, &proto, sizeof(proto)) < 0)
		return XDP_PASS;
	if (guard(ctx, 1, proto, ETH_HLEN) == V_DROP)
		return XDP_DROP;
	return XDP_PASS;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
