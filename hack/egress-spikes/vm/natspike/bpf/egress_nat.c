// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2026 Acnodal Inc.
//
// Phase 0 spike 2 follow-up for PureLB egress -- NOT production code.
//
//   pl_pod_out       tcx egress on a host NIC (design 6.4), after routing:
//                    PureLB-owned SNAT of a selected pod's forwarded packets
//                    into the node's port slice (D32). The pod is found by
//                    the device the packet entered the node on
//                    (skb->ingress_ifindex, the pod's host-side veth).
//   pl_pod_reply     tcx ingress where replies to the egress address arrive:
//                    reverse NAT, including the quoted header of ICMP errors.
//   pl_announce_all  one program for both pl_announce roles: the uplink path,
//                    or the tunnel-ingress path when skb->ifindex is the
//                    configured tunnel (the spike's pl_tun_in), so the
//                    verifier checks both in one program.
//
// The NAT is PureLB's own: no conntrack, no kfuncs. Fragments are dropped
// and counted (the "drop and count" option of D32).
//
// The coding rules spike 2 found for loading without CAP_PERFMON hold here
// too: variable-offset packet bytes go through bpf_skb_load_bytes /
// bpf_skb_store_bytes and the checksum helpers; everything built field by
// field lives in the per-CPU scratch map; values are selected, not
// addresses; __sk_buff->mark is written as 32 bits.
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/if_packet.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define MARK_FIELD  0xffu
#define REPLY_FLAG  0x80u
#define AF_INET_    2
#define AF_INET6_   10
#define MAX_NODES   256
#define GENEVE_VNI  1
#define IPPROTO_ICMPV6_ 58
#define NEXTHDR_FRAG_   44
#define NAT_PROBES  64      // bounded port probing within the slice

// ---- announce structures (copied from ../../bpf/egress_core.c) ------------

struct peer {
	__u32 addr[4]; // network order; IPv4 in addr[0]
	__u8 v6;
	__u8 pad[3];
};

struct addr_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad[3];
};

struct addr_cfg {
	__u16 base;
	__u8 shift;
	__u8 announced;
};

struct slice_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad;
	__u16 index;
};

struct config {
	__u32 self_node;
	__u32 tun_ifindex;
	__u32 pool_ifindex;
	__u32 l2_mode;
	__u32 push_eth;
	__u8 tun_mac[6];
	__u8 pad[2];
};

struct flow {
	struct addr_key dst;
	struct addr_key src;
	__u16 sport;
	__u16 dport;
	__u8 proto;
	__u8 ok;
	__u8 pad[2];
};

// ---- NAT structures ---------------------------------------------------------

// A selected pod, keyed by the host-side device's ifindex (like pod_devs);
// pl_pod_out looks it up by skb->ingress_ifindex.
struct pod_cfg {
	__u32 v4;        // the pod's addresses, network order
	__u32 v6[4];
	__u32 eg4;       // the egress addresses
	__u32 eg6[4];
	__u16 start;     // this node's slice: [start, start+size)
	__u16 size;      // a power of two
};

enum { NAT_OUT = 1, NAT_IN = 2 };
enum { NS_NEW = 1, NS_EST, NS_FIN, NS_CLOSED };

// One map holds both directions. NAT_IN is keyed by the translated tuple
// (remote, egress address, translated port) and holds the original source
// plus the flow state; NAT_OUT is the reverse entry, keyed by the original
// tuple, holding the translated port. Ports are host order; ICMP echo uses
// rport 0 and the identifier as lport.
struct nat_key {
	__u32 raddr[4];
	__u32 laddr[4];
	__u16 rport;
	__u16 lport;
	__u8 proto;
	__u8 family;
	__u8 dir;
	__u8 pad;
};

struct nat_val {
	__u32 addr[4];
	__u64 ts;        // bpf_ktime_get_ns of the last packet (GC input)
	__u16 port;
	__u8 state;      // NS_* (TCP-state-aware expiry)
	__u8 pad[5];
};

enum { K_NONE, K_PORTS, K_ECHO_REQ, K_ECHO_REP, K_ERR };

struct scratch {
	// announce
	struct bpf_tunnel_key key;
	struct flow f;
	struct slice_key sk;
	struct peer outer;
	__u8 hdr[40];
	__u8 l4[8];
	// NAT
	struct nat_key k;    // original tuple (NAT_OUT)
	struct nat_key xk;   // translated tuple (NAT_IN)
	struct nat_val v;
	struct addr_key ak;
	__u32 src[4];        // outer addresses
	__u32 dst[4];
	__u32 ir[4];         // ICMP error: quoted destination (the remote)
	__u32 o[4];          // bpf_csum_diff buffers: old and new address
	__u32 n[4];
	__u32 ow, nw;        // old and new port, as a 32-bit word
	__u32 co, cn;        // old and new quoted checksums, as a 32-bit word
	__u32 l4off;
	__u16 sport, dport;  // host order; echo: both are the identifier
	__u16 np;            // network-order port for bpf_skb_store_bytes
	__u16 ics;           // quoted L4 checksum
	__u16 nipcs, nics;   // rewritten quoted checksums
	__u8 ihdr[40];       // ICMP error: quoted IP header
	__u8 il4[8];         // ICMP error: quoted L4 header
	__u32 ph, pmask;     // port probing: flow hash, slice size - 1
	__u16 pstart, xport; // slice start; the port the probe claimed
	__u8 v6, proto, frag, kind, tcpflags, found;
	__u8 pad[2];
};

enum {
	ST_STEERED,
	ST_STEER_NO_ANNOUNCER,
	ST_TO_TUNNEL,
	ST_OWNER_LOCAL,
	ST_NO_OWNER,
	ST_TRANSIT,
	ST_TRANSIT_FORGED,
	ST_TRANSIT_TTL,
	ST_REPLY_LOCAL,
	ST_REPLY_NOT_OWNER,
	ST_PKT_TYPE_HOST,
	ST_PKT_TYPE_OTHER,
	ST_POD_TAGGED,
	ST_POD_SPOOF,
	ST_TRANSIT_NO_SENDER,
	ST_NAT_OUT,          // 15
	ST_NAT_NEW,
	ST_NAT_REPLY,
	ST_NAT_ICMP_ERR,
	ST_NAT_FRAG_DROP,
	ST_NAT_NO_PORT,
	ST_NAT_MISS,
	ST_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct addr_key);
	__type(value, struct addr_cfg);
} egress_addrs SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct slice_key);
	__type(value, __u32);
} slices SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, MAX_NODES);
	__type(key, __u32);
	__type(value, struct peer);
} nodes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_NODES);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct peer);
	__type(value, __u32);
} node_by_addr SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, __u32);
	__type(value, struct pod_cfg);
} pod_nat SEC(".maps");

// The same pods keyed by their address: on netkit the packet reaches the NIC
// with ingress_ifindex 0, so pl_pod_out finds the pod by its source instead.
// Safe only because pl_pod_tag drops spoofed sources at the netkit peer hook,
// ahead of the CNI.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct addr_key);
	__type(value, struct pod_cfg);
} pod_src SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 131072);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct nat_key);
	__type(value, struct nat_val);
} nat SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct config);
} cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct scratch);
} scratch SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, ST_MAX);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

static __always_inline void set_mark(struct __sk_buff *skb, __u32 field)
{
	__u32 m = (skb->mark & ~MARK_FIELD) | (field & MARK_FIELD);

	asm volatile("" : "+r"(m)); // keep the 4-byte store (spike 2 rule 6)
	skb->mark = m;
}

static __always_inline void count(__u32 i)
{
	__u64 *v = bpf_map_lookup_elem(&stats, &i);
	if (v)
		*v += 1;
}

static __always_inline struct config *config(void)
{
	__u32 z = 0;
	return bpf_map_lookup_elem(&cfg, &z);
}

static __always_inline struct scratch *scr(void)
{
	__u32 z = 0;
	return bpf_map_lookup_elem(&scratch, &z);
}

// ---- checksum arithmetic ----------------------------------------------------

static __always_inline __u32 csum_add(__u32 a, __u32 b)
{
	__u32 r = a + b;
	return r + (r < a);
}

// The kernel's csum_replace_by_diff on a raw 16-bit checksum field, for the
// checksums inside an ICMP error's quoted packet, which no helper reaches.
static __always_inline __u16 csum_apply(__u16 check, __u32 diff)
{
	__u32 s = csum_add(diff, ~(__u32)check);

	s = (s & 0xffff) + (s >> 16);
	s = (s & 0xffff) + (s >> 16);
	return (__u16)~s;
}

// Select a value without letting clang turn the choice into a select between
// two addresses, which the verifier refuses without CAP_PERFMON ("tried to
// add from different maps, paths or scalars"; spike 2 rule 5): both values
// are loaded unconditionally, then masked.
static __always_inline __u32 pick(int c, __u32 a, __u32 b)
{
	__u32 m = -(__u32)!!c;

	asm volatile("" : "+r"(a), "+r"(b));
	return (a & m) | (b & ~m);
}

// ---- NAT parsing --------------------------------------------------------------

// Parse the IPv4/IPv6 header at ETH_HLEN and the first 8 L4 bytes into
// scratch. IPv6 extension headers other than the fragment header are out of
// spike scope (such packets classify as K_NONE and pass untranslated).
static __always_inline int nparse(struct __sk_buff *skb, struct scratch *s)
{
	__u8 *h = s->hdr;
	__u32 l3 = ETH_HLEN;
	__u8 t;

	s->frag = 0;
	s->kind = K_NONE;
	s->tcpflags = 0;
	s->sport = s->dport = 0;
	__builtin_memset(s->src, 0, sizeof(s->src));
	__builtin_memset(s->dst, 0, sizeof(s->dst));
	if (skb->protocol == bpf_htons(ETH_P_IP)) {
		if (bpf_skb_load_bytes(skb, l3, h, 20) < 0)
			return -1;
		if ((h[0] >> 4) != 4 || (h[0] & 0xf) < 5)
			return -1;
		s->v6 = 0;
		s->proto = h[9];
		s->frag = ((h[6] & 0x3f) | h[7]) != 0; // MF or a fragment offset
		__builtin_memcpy(&s->src[0], h + 12, 4);
		__builtin_memcpy(&s->dst[0], h + 16, 4);
		s->l4off = l3 + (h[0] & 0xf) * 4;
	} else if (skb->protocol == bpf_htons(ETH_P_IPV6)) {
		if (bpf_skb_load_bytes(skb, l3, h, 40) < 0)
			return -1;
		s->v6 = 1;
		s->proto = h[6];
		s->frag = h[6] == NEXTHDR_FRAG_;
		__builtin_memcpy(s->src, h + 8, 16);
		__builtin_memcpy(s->dst, h + 24, 16);
		s->l4off = l3 + 40;
	} else {
		return -1;
	}
	if (s->frag)
		return 0;
	if (s->proto != IPPROTO_TCP && s->proto != IPPROTO_UDP &&
	    s->proto != IPPROTO_ICMP && s->proto != IPPROTO_ICMPV6_)
		return 0;
	if (bpf_skb_load_bytes(skb, s->l4off, s->l4, 8) < 0)
		return -1;
	if (s->proto == IPPROTO_TCP || s->proto == IPPROTO_UDP) {
		s->sport = (__u16)s->l4[0] << 8 | s->l4[1];
		s->dport = (__u16)s->l4[2] << 8 | s->l4[3];
		s->kind = K_PORTS;
		if (s->proto == IPPROTO_TCP &&
		    bpf_skb_load_bytes(skb, s->l4off + 13, &s->tcpflags, 1) < 0)
			return -1;
		return 0;
	}
	// ICMP only within its own family.
	if ((s->proto == IPPROTO_ICMPV6_) != s->v6)
		return 0;
	t = s->l4[0];
	s->sport = (__u16)s->l4[4] << 8 | s->l4[5];
	s->dport = s->sport;
	if (t == (s->v6 ? 128 : 8))
		s->kind = K_ECHO_REQ;
	else if (t == (s->v6 ? 129 : 0))
		s->kind = K_ECHO_REP;
	else if (s->v6 ? (t >= 1 && t <= 4) : (t == 3 || t == 11 || t == 12))
		s->kind = K_ERR;
	return 0;
}

static __always_inline void mkkey(struct nat_key *k, const __u32 *r, const __u32 *l,
				  __u16 rport, __u16 lport, __u8 proto, __u8 v6, __u8 dir)
{
	__builtin_memset(k, 0, sizeof(*k));
	__builtin_memcpy(k->raddr, r, 16);
	__builtin_memcpy(k->laddr, l, 16);
	k->rport = rport;
	k->lport = lport;
	k->proto = proto;
	k->family = v6 ? AF_INET6_ : AF_INET_;
	k->dir = dir;
}

static __always_inline __u8 next_state(__u8 st, __u8 proto, __u8 fl, int reply)
{
	if (proto == IPPROTO_TCP) {
		if (fl & 0x04) // RST
			return NS_CLOSED;
		if (fl & 0x01) // FIN
			return st == NS_CLOSED ? st : NS_FIN;
	}
	if (reply && st == NS_NEW)
		return NS_EST;
	return st;
}

// Rewrite the source (dst == 0) or destination (dst == 1) address to s->n
// and the port or echo identifier to nport, fixing the IPv4 header checksum
// and the L4 checksum (TCP and UDP and ICMPv6 include the pseudo header,
// ICMPv4 does not).
//
// Offsets and old values are selected as scalars (spike 2 rule 5); the
// address change is one bpf_csum_diff over 16 bytes for both families (an
// IPv4 address is word 0, the other words are zero in both buffers).
static __always_inline int xlate(struct __sk_buff *skb, struct scratch *s, int dst, __u16 nport)
{
	__u32 l3 = ETH_HLEN, l4 = s->l4off;
	__u8 v6 = s->v6, p = s->proto;
	int icmp = p == IPPROTO_ICMP || p == IPPROTO_ICMPV6_;
	__u32 aoff = l3 + (v6 ? (dst ? 24 : 8) : (dst ? 16 : 12));
	__u32 csoff = l4 + (p == IPPROTO_TCP ? 16 : p == IPPROTO_UDP ? 6 : 2);
	__u32 poff = l4 + (icmp ? 4 : (dst ? 2 : 0));
	__u64 mz = p == IPPROTO_UDP ? BPF_F_MARK_MANGLED_0 : 0;
	__u16 oport = pick(dst, s->dport, s->sport);
	__s64 diff;
	int i;

	for (i = 0; i < 4; i++)
		s->o[i] = pick(dst, s->dst[i], s->src[i]);
	diff = bpf_csum_diff(s->o, 16, s->n, 16, 0);
	if (diff < 0)
		return -1;
	if (!v6 && bpf_l3_csum_replace(skb, l3 + 10, 0, diff, 0) < 0)
		return -1;
	if (p != IPPROTO_ICMP &&
	    bpf_l4_csum_replace(skb, csoff, 0, diff, BPF_F_PSEUDO_HDR | mz) < 0)
		return -1;
	if (bpf_skb_store_bytes(skb, aoff, s->n, v6 ? 16 : 4, 0) < 0)
		return -1;
	if (oport != nport) {
		s->np = bpf_htons(nport);
		if (bpf_l4_csum_replace(skb, csoff, bpf_htons(oport), s->np, 2 | mz) < 0)
			return -1;
		if (bpf_skb_store_bytes(skb, poff, &s->np, 2, 0) < 0)
			return -1;
	}
	return 0;
}

// ---- pl_pod_out: tcx egress on a host NIC ------------------------------------

// One probe: claim candidate i of the slice. BPF_NOEXIST makes the insert of
// the translated tuple (s->xk, value s->v) the claim on the port.
static long probe_port(__u32 i, void *ctx)
{
	struct scratch *s = scr();
	__u16 cand;

	if (!s)
		return 1;
	// Double hashing: the start is the hash's low bits, the step its high
	// bits made odd, so the sequence visits the whole power-of-two slice
	// without the clustering linear probing builds up.
	cand = s->pstart + ((s->ph + i * ((s->ph >> 16) | 1)) & s->pmask);
	s->xk.lport = cand;
	if (bpf_map_update_elem(&nat, &s->xk, &s->v, BPF_NOEXIST) == 0) {
		s->xport = cand;
		s->found = 1;
		return 1;
	}
	return 0;
}

SEC("tcx/egress")
int pl_pod_out(struct __sk_buff *skb)
{
	// After routing, on the NIC: the pod is identified by the device its
	// packet entered the node on (skb_iif survives forwarding). Packets the
	// node originates have no ingress device and never match.
	__u32 ifx = skb->ingress_ifindex, mask;
	struct pod_cfg *pn = bpf_map_lookup_elem(&pod_nat, &ifx);
	struct scratch *s;
	struct nat_val *v;
	__u16 rport, xport = 0;
	__u64 now = bpf_ktime_get_ns();
	__u8 v6;

	s = scr();
	if (!s)
		return TCX_NEXT;
	if (!pn) {
		// No ingress device (netkit): find the pod by its source address.
		if (nparse(skb, s) < 0)
			return TCX_NEXT;
		s->ak.addr[0] = s->src[0];
		s->ak.addr[1] = s->v6 ? s->src[1] : 0;
		s->ak.addr[2] = s->v6 ? s->src[2] : 0;
		s->ak.addr[3] = s->v6 ? s->src[3] : 0;
		s->ak.family = s->v6 ? AF_INET6_ : AF_INET_;
		pn = bpf_map_lookup_elem(&pod_src, &s->ak);
		if (!pn)
			return TCX_NEXT; // not a selected pod
	} else if (nparse(skb, s) < 0) {
		return TCX_NEXT;
	}
	v6 = s->v6;
	// Only the pod's own address is translated; link-local and DAD (ND)
	// traffic passes. Source validation is pl_pod_tag's job.
	if (v6 ? (s->src[0] != pn->v6[0] || s->src[1] != pn->v6[1] ||
		  s->src[2] != pn->v6[2] || s->src[3] != pn->v6[3])
	       : s->src[0] != pn->v4)
		return TCX_NEXT;
	if (s->frag) {
		count(ST_NAT_FRAG_DROP);
		return TCX_DROP; // D32: drop and count
	}
	if (s->kind != K_PORTS && s->kind != K_ECHO_REQ)
		return TCX_NEXT; // ND, other ICMP, other protocols

	s->n[0] = pick(v6, pn->eg6[0], pn->eg4);
	s->n[1] = pick(v6, pn->eg6[1], 0);
	s->n[2] = pick(v6, pn->eg6[2], 0);
	s->n[3] = pick(v6, pn->eg6[3], 0);
	rport = s->kind == K_PORTS ? s->dport : 0;
	mkkey(&s->k, s->dst, s->src, rport, s->sport, s->proto, v6, NAT_OUT);
	mkkey(&s->xk, s->dst, s->n, rport, 0, s->proto, v6, NAT_IN);

	v = bpf_map_lookup_elem(&nat, &s->k);
	if (v) {
		xport = v->port;
	} else {
		mask = (__u32)pn->size - 1;
		if (!pn->size || (pn->size & mask))
			return TCX_DROP;
		__builtin_memcpy(s->v.addr, s->src, 16);
		s->v.port = s->sport;
		s->v.state = NS_NEW;
		s->v.ts = now;
		// Bounded probing (start and odd step from the flow hash), in bpf_loop: as a
		// plain bounded loop, every iteration's exit carried the rest of the
		// program as a separate verifier path, which 6.6, 6.8 and 6.12
		// rejected as too complex. The callback is verified once.
		s->ph = bpf_get_hash_recalc(skb);
		s->pmask = mask;
		s->pstart = pn->start;
		s->found = 0;
		bpf_loop(NAT_PROBES, probe_port, 0, 0);
		if (!s->found) {
			count(ST_NAT_NO_PORT);
			return TCX_DROP;
		}
		xport = s->xport;
		__builtin_memcpy(s->v.addr, s->n, 16);
		s->v.port = xport;
		if (bpf_map_update_elem(&nat, &s->k, &s->v, BPF_ANY) < 0) {
			bpf_map_delete_elem(&nat, &s->xk);
			return TCX_DROP;
		}
		count(ST_NAT_NEW);
	}
	s->xk.lport = xport;
	v = bpf_map_lookup_elem(&nat, &s->xk);
	if (v) {
		v->ts = now;
		v->state = next_state(v->state, s->proto, s->tcpflags, 0);
	}
	if (xlate(skb, s, 0, xport) < 0)
		return TCX_DROP;
	count(ST_NAT_OUT);
	return TCX_NEXT;
}

// ---- pl_pod_reply ---------------------------------------------------------------

// An ICMP error to the egress address quoting a translated packet: rewrite
// the outer destination, the quoted source address and port (or echo id),
// the quoted IPv4 header checksum and quoted L4 checksum, and fix the ICMP
// checksum by the sum of every quoted change (plus the pseudo header for
// ICMPv6).
static __always_inline int icmp_err(struct __sk_buff *skb, struct scratch *s)
{
	__u32 l3 = ETH_HLEN, l4 = s->l4off, il3 = l4 + 8, il4, csoff, poff;
	__u8 v6 = s->v6, *ih = s->ihdr, ip;
	__u16 xp, rp, op, oipcs = 0;
	struct nat_val *v;
	__s64 adiff, pdiff, cdiff;
	__u32 total, ldiff;
	int i, have_ics = 0;

	if (bpf_skb_load_bytes(skb, il3, ih, v6 ? 40 : 20) < 0)
		return TCX_NEXT;
	if (v6) {
		if ((ih[0] >> 4) != 6)
			return TCX_NEXT;
		ip = ih[6];
		il4 = il3 + 40;
	} else {
		if ((ih[0] >> 4) != 4 || (ih[0] & 0xf) < 5)
			return TCX_NEXT;
		ip = ih[9];
		il4 = il3 + (ih[0] & 0xf) * 4;
	}
	if (bpf_skb_load_bytes(skb, il4, s->il4, 8) < 0)
		return TCX_NEXT;
	if (ip == IPPROTO_TCP || ip == IPPROTO_UDP) {
		xp = (__u16)s->il4[0] << 8 | s->il4[1];
		rp = (__u16)s->il4[2] << 8 | s->il4[3];
		poff = 0;
		csoff = ip == IPPROTO_TCP ? 16 : 6;
	} else if (ip == (v6 ? IPPROTO_ICMPV6_ : IPPROTO_ICMP) && s->il4[0] == (v6 ? 128 : 8)) {
		xp = (__u16)s->il4[4] << 8 | s->il4[5];
		rp = 0;
		poff = 4;
		csoff = 2;
	} else {
		return TCX_NEXT;
	}
	__builtin_memset(s->o, 0, 16);
	__builtin_memset(s->ir, 0, 16);
	if (v6) {
		__builtin_memcpy(s->o, ih + 8, 16);
		__builtin_memcpy(s->ir, ih + 24, 16);
	} else {
		__builtin_memcpy(&s->o[0], ih + 12, 4);
		__builtin_memcpy(&s->ir[0], ih + 16, 4);
		oipcs = (__u16)ih[10] | (__u16)ih[11] << 8; // raw, as in memory
	}
	// The error must be addressed to the quoted (translated) source.
	for (i = 0; i < 4; i++)
		if (s->o[i] != s->dst[i])
			return TCX_NEXT;
	mkkey(&s->xk, s->ir, s->o, rp, xp, ip, v6, NAT_IN);
	v = bpf_map_lookup_elem(&nat, &s->xk);
	if (!v) {
		count(ST_NAT_MISS);
		return TCX_NEXT;
	}
	__builtin_memcpy(s->n, v->addr, 16);
	op = v->port;

	adiff = bpf_csum_diff(s->o, 16, s->n, 16, 0);
	s->ow = bpf_htons(xp);
	s->nw = bpf_htons(op);
	pdiff = bpf_csum_diff(&s->ow, 4, &s->nw, 4, 0);
	if (adiff < 0 || pdiff < 0)
		return TCX_DROP;
	// The quoted L4 checksum: pseudo header (not for ICMPv4) and port.
	ldiff = csum_add(pdiff, ip == IPPROTO_ICMP ? 0 : adiff);
	if (bpf_skb_load_bytes(skb, il4 + csoff, &s->ics, 2) == 0 &&
	    !(ip == IPPROTO_UDP && s->ics == 0)) {
		have_ics = 1;
		s->nics = csum_apply(s->ics, ldiff);
		if (ip == IPPROTO_UDP && s->nics == 0)
			s->nics = 0xffff;
	} else {
		s->ics = s->nics = 0;
	}
	s->nipcs = v6 ? 0 : csum_apply(oipcs, adiff);
	s->co = (__u32)(v6 ? 0 : oipcs) | (__u32)s->ics << 16;
	s->cn = (__u32)s->nipcs | (__u32)s->nics << 16;
	cdiff = bpf_csum_diff(&s->co, 4, &s->cn, 4, 0);
	if (cdiff < 0)
		return TCX_DROP;
	total = csum_add(csum_add(adiff, pdiff), cdiff);

	// Outer destination, and its IPv4 header checksum.
	if (bpf_skb_store_bytes(skb, l3 + (v6 ? 24 : 16), s->n, v6 ? 16 : 4, 0) < 0)
		return TCX_DROP;
	if (!v6 && bpf_l3_csum_replace(skb, l3 + 10, 0, adiff, 0) < 0)
		return TCX_DROP;
	// Quoted source address, port, and checksums.
	if (bpf_skb_store_bytes(skb, il3 + (v6 ? 8 : 12), s->n, v6 ? 16 : 4, 0) < 0)
		return TCX_DROP;
	s->np = bpf_htons(op);
	if (bpf_skb_store_bytes(skb, il4 + poff, &s->np, 2, 0) < 0)
		return TCX_DROP;
	if (!v6 && bpf_skb_store_bytes(skb, il3 + 10, &s->nipcs, 2, 0) < 0)
		return TCX_DROP;
	if (have_ics && bpf_skb_store_bytes(skb, il4 + csoff, &s->nics, 2, 0) < 0)
		return TCX_DROP;
	// The ICMP checksum covers all of the above as payload; ICMPv6 also
	// covers the outer destination through its pseudo header.
	if (bpf_l4_csum_replace(skb, l4 + 2, 0, total, 0) < 0)
		return TCX_DROP;
	if (v6 && bpf_l4_csum_replace(skb, l4 + 2, 0, adiff, BPF_F_PSEUDO_HDR) < 0)
		return TCX_DROP;
	count(ST_NAT_ICMP_ERR);
	return TCX_NEXT;
}

SEC("tcx/ingress")
int pl_pod_reply(struct __sk_buff *skb)
{
	struct scratch *s = scr();
	struct nat_val *v;
	__u16 rport, port;

	if (!s || nparse(skb, s) < 0)
		return TCX_NEXT;
	if (s->frag) {
		__builtin_memset(&s->ak, 0, sizeof(s->ak));
		__builtin_memcpy(s->ak.addr, s->dst, 16);
		s->ak.family = s->v6 ? AF_INET6_ : AF_INET_;
		if (bpf_map_lookup_elem(&egress_addrs, &s->ak)) {
			count(ST_NAT_FRAG_DROP);
			return TCX_DROP; // D32: drop and count
		}
		return TCX_NEXT;
	}
	if (s->kind == K_ERR)
		return icmp_err(skb, s);
	if (s->kind != K_PORTS && s->kind != K_ECHO_REP)
		return TCX_NEXT;
	rport = s->kind == K_PORTS ? s->sport : 0;
	mkkey(&s->xk, s->src, s->dst, rport, s->dport, s->proto, s->v6, NAT_IN);
	v = bpf_map_lookup_elem(&nat, &s->xk);
	if (!v)
		return TCX_NEXT; // not a translated flow
	__builtin_memcpy(s->n, v->addr, 16);
	port = v->port;
	v->ts = bpf_ktime_get_ns();
	v->state = next_state(v->state, s->proto, s->tcpflags, 1);
	if (xlate(skb, s, 1, port) < 0)
		return TCX_DROP;
	count(ST_NAT_REPLY);
	return TCX_NEXT;
}

// ---- pl_announce_all: uplink and purelb-eg0 in one program -----------------
// Copied from ../../bpf/egress_core.c (pl_announce, pl_tun_in); see there for
// why each shape is what it is.

static __always_inline int set_key(struct __sk_buff *skb, struct scratch *s, const struct peer *p)
{
	struct bpf_tunnel_key *k = &s->key;
	__u32 w0 = p->addr[0];
	__u64 flags = 0;

	__builtin_memset(k, 0, sizeof(*k));
	k->tunnel_id = GENEVE_VNI;
	k->tunnel_ttl = 64;
	if (!p->v6)
		w0 = bpf_ntohl(w0);
	k->remote_ipv6[0] = w0;
	if (p->v6) {
		k->remote_ipv6[1] = p->addr[1];
		k->remote_ipv6[2] = p->addr[2];
		k->remote_ipv6[3] = p->addr[3];
		flags = BPF_F_TUNINFO_IPV6;
	}
	return bpf_skb_set_tunnel_key(skb, k, sizeof(*k), flags);
}

static __always_inline int parse(struct __sk_buff *skb, struct scratch *s, __u32 l3,
				 __u16 proto_be)
{
	struct flow *f = &s->f;
	__u8 *h = s->hdr;
	__u32 l4;

	__builtin_memset(f, 0, sizeof(*f));
	if (proto_be == bpf_htons(ETH_P_IP)) {
		if (bpf_skb_load_bytes(skb, l3, h, 20) < 0)
			return -1;
		if ((h[0] >> 4) != 4 || (h[0] & 0xf) < 5)
			return -1;
		if (((h[6] & 0x1f) | h[7]) != 0)
			return -1;
		f->dst.family = AF_INET_;
		__builtin_memcpy(&f->dst.addr[0], h + 16, 4);
		f->src.family = AF_INET_;
		__builtin_memcpy(&f->src.addr[0], h + 12, 4);
		f->proto = h[9];
		l4 = l3 + (h[0] & 0xf) * 4;
	} else if (proto_be == bpf_htons(ETH_P_IPV6)) {
		if (bpf_skb_load_bytes(skb, l3, h, 40) < 0)
			return -1;
		f->dst.family = AF_INET6_;
		__builtin_memcpy(f->dst.addr, h + 24, 16);
		f->src.family = AF_INET6_;
		__builtin_memcpy(f->src.addr, h + 8, 16);
		f->proto = h[6];
		l4 = l3 + 40;
	} else {
		return -1;
	}

	if (f->proto != IPPROTO_TCP && f->proto != IPPROTO_UDP &&
	    f->proto != IPPROTO_ICMP && f->proto != IPPROTO_ICMPV6_)
		return 0;
	if (bpf_skb_load_bytes(skb, l4, s->l4, 8) < 0)
		return -1;
	if (f->proto == IPPROTO_TCP || f->proto == IPPROTO_UDP) {
		f->sport = (__u16)s->l4[0] << 8 | s->l4[1];
		f->dport = (__u16)s->l4[2] << 8 | s->l4[3];
		f->ok = 1;
	} else {
		__u8 type = s->l4[0];

		if ((f->proto == IPPROTO_ICMP && (type == 0 || type == 8)) ||
		    (f->proto == IPPROTO_ICMPV6_ && (type == 129 || type == 128))) {
			f->sport = (__u16)s->l4[4] << 8 | s->l4[5];
			f->dport = f->sport;
			f->ok = 1;
		} else if (f->proto == IPPROTO_ICMPV6_ && type >= 133 && type <= 137) {
			f->ok = 2;
		}
	}
	return 0;
}

static __always_inline __u32 *slice_owner(struct scratch *s, const struct addr_key *a,
					   const struct addr_cfg *c, __u16 port)
{
	struct slice_key *k = &s->sk;

	if (port < c->base)
		return 0;
	__builtin_memset(k, 0, sizeof(*k));
	__builtin_memcpy(k->addr, a->addr, 16);
	k->family = a->family;
	k->index = port >> c->shift;
	return bpf_map_lookup_elem(&slices, k);
}

static __always_inline int dec_hop(struct __sk_buff *skb, struct scratch *s, __u32 l3, __u16 proto_be)
{
	__u8 *h = s->hdr;

	if (proto_be == bpf_htons(ETH_P_IP)) {
		__u8 ttl = h[8];
		__be16 old, new;

		if (ttl <= 1)
			return -1;
		old = bpf_htons((__u16)ttl << 8 | h[9]);
		new = bpf_htons((__u16)(ttl - 1) << 8 | h[9]);
		ttl--;
		if (bpf_l3_csum_replace(skb, l3 + 10, old, new, 2) < 0)
			return -1;
		return bpf_skb_store_bytes(skb, l3 + 8, &ttl, 1, 0) < 0 ? -1 : 0;
	}
	if (proto_be == bpf_htons(ETH_P_IPV6)) {
		__u8 hop = h[7];

		if (hop <= 1)
			return -1;
		hop--;
		return bpf_skb_store_bytes(skb, l3 + 7, &hop, 1, 0) < 0 ? -1 : 0;
	}
	return -1;
}

// The uplink role (the spike's pl_announce).
static __always_inline int announce_uplink(struct __sk_buff *skb, struct config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	struct addr_cfg *ac;
	struct peer *p;
	__u32 *owner;

	if (parse(skb, s, ETH_HLEN, skb->protocol) < 0)
		return TCX_NEXT;
	ac = bpf_map_lookup_elem(&egress_addrs, &f->dst);
	if (!ac)
		return TCX_NEXT;
	if (f->ok != 1)
		return TCX_NEXT;
	owner = slice_owner(s, &f->dst, ac, f->dport);
	if (!owner || *owner == 0) {
		count(ST_NO_OWNER);
		return TCX_DROP;
	}
	if (*owner == c->self_node) {
		count(ST_OWNER_LOCAL);
		return TCX_NEXT;
	}
	p = bpf_map_lookup_elem(&nodes, owner);
	if (!p || set_key(skb, s, p) < 0)
		return TCX_DROP;
	count(ST_TO_TUNNEL);
	return bpf_redirect(c->tun_ifindex, 0);
}

// The purelb-eg0 role (the spike's pl_tun_in).
static __always_inline int announce_tunnel(struct __sk_buff *skb, struct config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	struct addr_cfg *ac;
	__u32 l3 = 0, *owner, *sender;
	__u16 proto = skb->protocol;

	if (c->l2_mode) {
		void *data = (void *)(long)skb->data;
		void *end = (void *)(long)skb->data_end;
		struct ethhdr *eth = data;

		if ((void *)(eth + 1) > end)
			return TCX_DROP;
		proto = eth->h_proto;
		__builtin_memcpy(eth->h_dest, c->tun_mac, 6);
		bpf_skb_change_type(skb, PACKET_HOST);
		l3 = ETH_HLEN;
	}
	if (skb->pkt_type == PACKET_HOST)
		count(ST_PKT_TYPE_HOST);
	else
		count(ST_PKT_TYPE_OTHER);

	if (parse(skb, s, l3, proto) < 0)
		return TCX_NEXT;

	ac = bpf_map_lookup_elem(&egress_addrs, &f->src);
	if (ac && ac->announced) {
		struct peer *outer = &s->outer;

		if (f->ok != 1)
			return TCX_DROP;
		__builtin_memset(outer, 0, sizeof(*outer));
		if (bpf_skb_get_tunnel_key(skb, &s->key, sizeof(s->key), 0) == 0 && s->key.remote_ipv4) {
			outer->addr[0] = bpf_htonl(s->key.remote_ipv4);
		} else if (bpf_skb_get_tunnel_key(skb, &s->key, sizeof(s->key), BPF_F_TUNINFO_IPV6) == 0) {
			outer->addr[0] = s->key.remote_ipv6[0];
			outer->addr[1] = s->key.remote_ipv6[1];
			outer->addr[2] = s->key.remote_ipv6[2];
			outer->addr[3] = s->key.remote_ipv6[3];
			outer->v6 = 1;
		}
		sender = bpf_map_lookup_elem(&node_by_addr, outer);
		if (!sender) {
			count(ST_TRANSIT_NO_SENDER);
			return TCX_DROP;
		}
		owner = slice_owner(s, &f->src, ac, f->sport);
		if (!owner || *owner != *sender) {
			count(ST_TRANSIT_FORGED);
			return TCX_DROP;
		}
		if (dec_hop(skb, s, l3, proto) < 0) {
			count(ST_TRANSIT_TTL);
			return TCX_DROP;
		}
		count(ST_TRANSIT);
		if (!c->l2_mode && c->push_eth) {
			void *data, *end;
			struct ethhdr *eth;

			if (bpf_skb_change_head(skb, ETH_HLEN, 0) < 0)
				return TCX_DROP;
			data = (void *)(long)skb->data;
			end = (void *)(long)skb->data_end;
			eth = data;
			if ((void *)(eth + 1) > end)
				return TCX_DROP;
			__builtin_memset(eth, 0, ETH_HLEN);
			eth->h_proto = proto;
		}
		return bpf_redirect_neigh(c->pool_ifindex, 0, 0, 0);
	}

	ac = bpf_map_lookup_elem(&egress_addrs, &f->dst);
	if (ac && f->ok == 1) {
		owner = slice_owner(s, &f->dst, ac, f->dport);
		if (!owner || *owner != c->self_node) {
			count(ST_REPLY_NOT_OWNER);
			return TCX_DROP;
		}
		set_mark(skb, REPLY_FLAG);
		count(ST_REPLY_LOCAL);
		return TCX_NEXT;
	}
	return TCX_DROP;
}

SEC("tcx/ingress")
int pl_announce_all(struct __sk_buff *skb)
{
	struct config *c = config();
	struct scratch *s = scr();

	if (!c || !s)
		return TCX_NEXT;
	if (c->tun_ifindex && skb->ifindex == c->tun_ifindex)
		return announce_tunnel(skb, c, s);
	return announce_uplink(skb, c, s);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
