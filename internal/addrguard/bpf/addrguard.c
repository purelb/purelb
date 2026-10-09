// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2026 Acnodal Inc.
//
// Address guard: drop packets addressed to a PureLB VIP unless their
// (protocol, port) is one of that VIP's Service ports.
//
// One parser, two entry points: tcx ingress (the default) and native XDP
// (opt-in). Packets not addressed to a guarded address take the fast path:
// one header read and one map lookup, uncounted.
//
// Every packet gets a verdict from what the guard has read: it never passes
// a packet because it couldn't read it. Headers are read wherever they lie
// in the packet (linear area or page fragments); a frame too short to hold
// the headers it claims, or with more VLAN tags than QinQ uses, is dropped
// and counted (ag_unread).
//
// tcx has two programs, one per kind of link: the network header's offset
// is a constant in each, so finding it costs nothing per packet (a BPF
// program can't read it from the skb without CAP_PERFMON). The agent
// attaches the one that fits the link, and refuses links of any other kind.

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

/* At most QinQ: two tags in all, counting one the kernel has already moved
 * to skb metadata before tcx runs. */
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

/* Index layout of ag_unread: reason * 2 + (0 drop, 1 would_drop). The
 * destination isn't known, so there is no family or per-VIP counter. */
enum unread_reason {
	U_TRUNCATED = 0,  /* shorter than the VLAN tag or IP header it claims */
	U_VLAN_DEPTH,     /* more VLAN tags than MAX_VLAN_TAGS */
	U_MAX,
};
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

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, U_MAX * 2);
	__type(key, __u32);
	__type(value, __u64);
} ag_unread SEC(".maps");


/* Verdict returned by the shared parser. */
enum verdict { V_PASS = 0, V_DROP = 1 };

/* Reads len bytes at off -- from the start of the packet as the hook sees
 * it -- into to. Both helpers read across page fragments, so a header
 * outside the linear area reads like any other, with no pull and no
 * allocation; they fail only when the packet is shorter than off + len.
 *
 * The buffers read after a packet has matched a VIP (extension headers,
 * ICMP type, ports) are zero-initialised by their callers. Linux 6.17's
 * verifier, for a loader without CAP_PERFMON (the agent has CAP_BPF and
 * CAP_NET_ADMIN only), wants a speculation barrier after a helper writes
 * into stack the program hasn't written yet, and then rejects the load
 * because a helper call is a jump ("verifier bug: speculation barrier after
 * jump instruction"). Writing the buffer first avoids that. Which loads
 * trigger it depends on which paths the verifier explores speculatively,
 * so on the code the compiler generates as well as on the kernel: with the
 * pinned clang 19, the buffers on the path every packet takes (VLAN tag,
 * IP header) don't, and are left alone -- zeroing them costs a barrier per
 * packet. A compiler or kernel change can move it: bpf-test loads the
 * program on CI's kernel. */
static __always_inline int load(void *ctx, int xdp, __u32 off, void *to, __u32 len)
{
	if (xdp)
		return bpf_xdp_load_bytes(ctx, off, to, len);
	return bpf_skb_load_bytes(ctx, off, to, len);
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

/* A frame the guard can't read (see enum unread_reason): dropped, or in
 * monitor mode counted as would_drop and passed. */
static __always_inline int unread(__u32 reason)
{
	int mon = monitoring();
	__u32 idx = reason * 2 + (mon ? 1 : 0);
	__u64 *c = bpf_map_lookup_elem(&ag_unread, &idx);

	if (c)
		*c += 1;
	return mon ? V_PASS : V_DROP;
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

/* ICMP error messages: they quote the packet that caused them. */
static __always_inline int icmp_error(__u8 type, __u32 fam)
{
	if (fam == FAM_V4)
		return type == 3 || type == 11 || type == 12;
	return type >= 1 && type <= 4;
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
 * header it describes from the start of the packet, both as the hook
 * presents them; tags is the number of VLAN tags already taken out of the
 * packet (tcx: the one the kernel moved to metadata). */
static __always_inline int guard(void *ctx, int xdp, __be16 proto, __u32 off, __u32 tags)
{
	struct vip_counters *vc;
	struct port_key pk;
	__u32 fam, l4off;
	__u8 l4proto;

	/* Skip in-band VLAN tags: each is [TCI][encapsulated ethertype]. With
	 * tcx the kernel has already moved the outer tag to metadata (tags), so
	 * at most one more is in the packet; XDP sees every tag in-band. */
#pragma unroll
	for (int i = 0; i < MAX_VLAN_TAGS; i++) {
		if (proto != ETH_P_8021Q_BE && proto != ETH_P_8021AD_BE)
			break;
		if (tags >= MAX_VLAN_TAGS)
			return unread(U_VLAN_DEPTH);
		__be16 tag[2];

		if (load(ctx, xdp, off, tag, sizeof(tag)) < 0)
			return unread(U_TRUNCATED);
		proto = tag[1];
		off += 4;
		tags++;
	}
	if (proto == ETH_P_8021Q_BE || proto == ETH_P_8021AD_BE)
		return unread(U_VLAN_DEPTH);

	if (proto == ETH_P_IP_BE) {
		struct iphdr ip;

		if (load(ctx, xdp, off, &ip, sizeof(ip)) < 0)
			return unread(U_TRUNCATED);
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
			return unread(U_TRUNCATED);
		vc = bpf_map_lookup_elem(&ag_vip6, &ip6.daddr);
		if (!vc)
			return V_PASS;
		fam = FAM_V6;
		if (ip6.version != 6)
			return decide(vc, 0, R_MALFORMED, fam);
		l4off = off + sizeof(ip6);
		l4proto = ip6.nexthdr;

#pragma unroll
		for (int i = 0; i < MAX_EXT_HDRS; i++) {
			if (l4proto == IPPROTO_FRAGMENT_) {
				__u8 h[4] = {}; /* next header, reserved, offset+flags */

				if (load(ctx, xdp, l4off, h, sizeof(h)) < 0)
					return decide(vc, 0, R_MALFORMED, fam);
				if ((((__u16)h[2] << 8) | h[3]) & 0xfff8)
					return decide(vc, 1, R_FRAGMENT, fam);
				l4proto = h[0];
				l4off += 8;
			} else if (is_ext_hdr(l4proto)) {
				__u8 h[2] = {}; /* next header, length in 8-octet units - 1 */

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
		/* Not IP: ARP, LLDP and the like, never addressed to a VIP. */
		return V_PASS;
	}

	if ((fam == FAM_V4 && l4proto == IPPROTO_ICMP) ||
	    (fam == FAM_V6 && l4proto == IPPROTO_ICMPV6_)) {
		__u8 type = 0;

		if (load(ctx, xdp, l4off, &type, sizeof(type)) < 0)
			return decide(vc, 0, R_MALFORMED, fam);
		if (!icmp_allowed(type, fam))
			return decide(vc, 0, R_ICMP_DENIED, fam);
		if (icmp_error(type, fam)) {
			/* An error quotes, after the 8-byte ICMP header, the packet
			 * that caused it, and the kernel acts on the quote: path
			 * MTU for the quoted destination, the socket matching the
			 * quoted addresses. An error about this VIP's own traffic
			 * quotes a packet from this VIP; any other source is
			 * someone else's traffic -- the node's own, say -- and the
			 * error is forged. */
			__u32 qsrc[4] = {};

			if (fam == FAM_V4) {
				if (load(ctx, xdp, l4off + 8 + 12, qsrc, 4) < 0)
					return decide(vc, 0, R_MALFORMED, fam);
				if (qsrc[0] != pk.addr[0])
					return decide(vc, 0, R_ICMP_DENIED, fam);
			} else {
				if (load(ctx, xdp, l4off + 8 + 8, qsrc, 16) < 0)
					return decide(vc, 0, R_MALFORMED, fam);
				if (qsrc[0] != pk.addr[0] || qsrc[1] != pk.addr[1] ||
				    qsrc[2] != pk.addr[2] || qsrc[3] != pk.addr[3])
					return decide(vc, 0, R_ICMP_DENIED, fam);
			}
		}
		return decide(vc, 1, R_ICMP, fam);
	}

	if (l4proto == IPPROTO_TCP || l4proto == IPPROTO_UDP || l4proto == IPPROTO_SCTP) {
		__be16 ports[2] = {}; /* source, destination: same layout in all three */

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

/* off is where the network header starts, from the start of the packet
 * as tcx sees it. A tag the kernel has moved to metadata counts towards
 * the VLAN limit. */
static __always_inline int tcx_guard(struct __sk_buff *skb, __u32 off)
{
	int v = guard(skb, 0, skb->protocol, off, skb->vlan_present ? 1 : 0);

	return v == V_DROP ? TCX_DROP : TCX_NEXT;
}

/* Ethernet links: the network header follows the 14-byte Ethernet header
 * (an inner QinQ tag, if any, is in-band at 14). */
SEC("tcx/ingress")
int ag_tcx(struct __sk_buff *skb)
{
	return tcx_guard(skb, ETH_HLEN);
}

/* Links with no link-layer header (IP tunnels, tun, WireGuard): the packet
 * starts with the network header. */
SEC("tcx/ingress")
int ag_tcx_l3(struct __sk_buff *skb)
{
	return tcx_guard(skb, 0);
}

SEC("xdp.frags")
int ag_xdp(struct xdp_md *ctx)
{
	__be16 proto;

	int v;

	if (bpf_xdp_load_bytes(ctx, 12, &proto, sizeof(proto)) < 0)
		v = unread(U_TRUNCATED);
	else
		v = guard(ctx, 1, proto, ETH_HLEN, 0);
	return v == V_DROP ? XDP_DROP : XDP_PASS;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
