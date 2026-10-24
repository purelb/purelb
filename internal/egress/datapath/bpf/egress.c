// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2026 Acnodal Inc.
//
// The announcer stage of PureLB egress (design 6.5).
//
// pl_announce runs at tcx ingress, behind the address guard, on two kinds of
// device:
//
//   an uplink (pl_announce_eth for Ethernet links, pl_announce_l3 for links
//   without a link-layer header): a reply to an egress address goes to the
//   node owning its port slice, through the tunnel, or, when that is this
//   node, on to the stack with the direct-path mark for its pool interface;
//
//   purelb-eg0 (pl_announce_tun): transit from a pod node goes out the
//   address's pool interface, and a reply for a pod on this node goes on to
//   the stack with the reply flag; anything else is forged and dropped.
//
// pl_steer runs at tcx egress of purelb-eg0 on a pod node (design 6.8): a
// packet whose mark carries a slot gets the tunnel key of one of the slot's
// holders, and the mark field is cleared so the outer route lookup can't
// match the slot rule again (ELOOP).
//
// Packets not addressed to an egress address take the fast path on an
// uplink: one header read and one map lookup, uncounted. Transit is
// redirected, never punted to the stack.
//
// Verifier rules (design 6.6): every program loads with CAP_BPF and
// CAP_NET_ADMIN only. Without CAP_PERFMON the verifier refuses variable-offset
// packet access, partial writes to stack slots that held a pointer,
// path-dependent pointer arithmetic, and (Linux 6.17, 6.18 < 6.18.14,
// 6.19 < 6.19.4) a helper writing stack bytes on a speculative path. So
// headers are copied with bpf_skb_load_bytes into the per-CPU scratch map,
// parse state lives there too, values are chosen rather than pointers, and
// loops run in bpf_loop.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/if_packet.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define AF_INET_  2
#define AF_INET6_ 10

#define ETH_P_8021Q_BE  bpf_htons(0x8100)
#define ETH_P_8021AD_BE bpf_htons(0x88A8)
#define ETH_P_IP_BE     bpf_htons(ETH_P_IP)
#define ETH_P_IPV6_BE   bpf_htons(ETH_P_IPV6)
#define MAX_VLAN_TAGS   2

#define IPPROTO_HOPOPTS_  0
#define IPPROTO_ROUTING_  43
#define IPPROTO_FRAGMENT_ 44
#define IPPROTO_AH_       51
#define IPPROTO_ICMPV6_   58
#define IPPROTO_DSTOPTS_  60
#define IPV6_MAX_EXT      8

// The mark field is one byte of skb->mark (config mark_shift picks which,
// D18). Slots are 1..127; the top bit is the reply flag: 0x80 for a reply
// that arrived through the tunnel, 0x81 + n for one that arrived directly on
// pool interface n (routing table 7900 + n).
#define REPLY_FLAG 0x80u
#define DIRECT_MAX 63

#define GENEVE_VNI  1
#define MAX_HOLDERS 32
#define FRAG_MISS  0xffffffffffffffffull // owner_of: a later fragment with no frags entry

#define PTB6_QUOTE  1232 // 1280 - 40 - 8: an ICMPv6 error fits the IPv6 minimum MTU
#define PTB4_QUOTE  548  // 576 - 20 - 8 (RFC 1812 4.3.2.3)
#define PTB_MIN_MTU 1280

// Counters: reason * 2 + family (0 IPv4, 1 IPv6).
enum reason {
	R_TO_TUNNEL = 0,    // uplink: a reply sent to its owner through the tunnel
	R_OWNER_LOCAL,      // uplink: a reply for a pod on this node
	R_SERVICE_PORT,     // uplink: a Service port of a shared address, to kube-proxy
	R_NOT_ANNOUNCER,    // uplink: an egress address this node doesn't hold
	R_NO_OWNER,         // a port no slice, reserved port or PerPod owner covers
	R_FRAG_MISSING,     // a later fragment whose first never came
	R_PTB_SENT,         // an oversize reply answered with PTB / frag-needed
	R_PTB_FAILED,       // a helper failed building one (dropped)
	R_NO_NODE,          // the owner isn't in nodes
	R_MALFORMED,        // addressed to an egress address but unparseable
	R_TRANSIT,          // tunnel: transit sent out the pool interface
	R_REPLY_LOCAL,      // tunnel: a reply for a pod on this node
	R_FORGED_TUNNEL,    // tunnel: the outer source isn't a node
	R_FORGED_INNER,     // tunnel: transit outside the sender's slice
	R_FORGED_INNER_DST, // tunnel: neither transit nor a reply this node owns
	R_TTL_EXPIRED,      // tunnel: transit whose TTL or hop limit would expire
	R_NO_ROUTE,         // tunnel: no route in the pool interface's table
	R_STEERED,          // steering: a slot's packet keyed to a holder
	R_NO_HOLDER,        // steering: a slot with no holder (dropped)
	R_MAX,
};

// An address: IPv4 in addr[0], the rest zero; family keeps the two families
// from aliasing.
struct addr_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad[3];
};

// What a node knows of one egress address.
struct addr_entry {
	__u64 whole_owner; // PerPod: the node that owns every port and protocol; 0: sliced
	__u32 table;       // the pool interface's routing table, for transit
	__u16 base;        // first slice port
	__u8 shift;        // log2(slice size); slices align on absolute ports
	__u8 held;         // this node announces or advertises the address
};

struct slice_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad;
	__u16 index; // port >> shift
};

// A Service port of a shared address (svc_ports) or a reserved port
// (reserved).
struct port_key {
	__u32 addr[4];
	__u16 port;
	__u8 proto;
	__u8 family;
};

// What every fragment of one datagram shares.
struct frag_key {
	__u32 src[4];
	__u32 dst[4];
	__u32 id; // IPv4: the 16-bit IP ID; IPv6: the 32-bit Fragment Header ID
	__u8 family;
	__u8 proto;
	__u8 pad[2];
};

// A node's underlay addresses (network order) and tunnel MAC (L2 mode).
struct node {
	__u32 v4;
	__u32 v6[4];
	__u8 mac[6];
	__u8 has_v4;
	__u8 has_v6;
};

// An underlay address, to find the node a tunnel packet came from.
struct underlay_key {
	__u32 addr[4];
	__u8 v6;
	__u8 pad[3];
};

// A slot's holders (steer): the elected owner of a local address, or a
// routed address's live advertisers (D1, D7).
struct holders {
	__u32 n;
	__u32 pad;
	__u64 holders[MAX_HOLDERS];
};

struct pool_if {
	__u32 n; // routing table 7900 + n; the direct-path mark is 0x81 + n
};

struct egress_config {
	__u64 self;        // this node's id
	__u32 tun_ifindex; // purelb-eg0
	__u32 tun_mtu;     // largest inner packet the tunnel carries; 0: no PTB
	__u8 tun_mac[6];   // purelb-eg0's MAC (L2 mode)
	__u8 l2;           // 1: L2 Geneve (no innerprotoinherit)
	__u8 mark_shift;   // 0, 8, 16 or 24: the mark byte (D18)
	__u8 has_v4;       // this node's underlay families
	__u8 has_v6;
	__u8 pad[6];
};

// ICMP and ICMPv6 echo share this layout.
struct icmp_echo {
	__u8 type;
	__u8 code;
	__be16 csum;
	__be16 id;
	__be16 seq;
};

struct flow {
	struct addr_key src;
	struct addr_key dst;
	__u16 sport; // source port, echo id, or an ICMP error's quoted destination port
	__u16 dport; // destination port, echo id, or an ICMP error's quoted source port
	__u8 proto;  // the upper-layer protocol (IPv6: after the extension headers)
	__u8 ported; // sport/dport are meaningful
	__u8 frag;   // 0: not a fragment, 1: first fragment, 2: later fragment
	__u8 fproto; // frags key protocol: IPv4 protocol, IPv6 Fragment Header next header
	__u32 frag_id;
	__u32 l4;    // offset of the upper-layer header
};

// Everything a program builds field by field, per CPU (see the top).
struct scratch {
	struct bpf_tunnel_key key;
	struct bpf_fib_lookup fib;
	struct bpf_redir_neigh nh;
	struct flow f;
	struct slice_key sk;
	struct port_key pk;
	struct frag_key fk;
	struct underlay_key uk;
	__u64 fval;
	__u64 owner;
	__u32 ifindex; // a pool_ifs key
	__u8 hdr[40];  // the IPv4 or IPv6 header
	__u8 l4b[8];   // ports, or the ICMP header
	__u8 qh[40];   // an ICMP error's quoted IP header
	__u8 qp[8];    // its quoted ports or echo header
	__u8 eh[8];    // the IPv6 extension header being walked
	__u32 w_off;   // extension header walk, across bpf_loop iterations
	__u8 w_nh;
	__u8 w_done;
	__u8 w_pad[2];
	__u8 eth[16];  // PTB: the arriving frame's Ethernet header
	__u8 ip[40];   // PTB: the new IP header
	__u8 icmp[8];  // PTB: the new ICMP header
	__u8 ps[40];   // PTB: the ICMPv6 pseudo header
	__u8 q[PTB6_QUOTE];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct addr_key);
	__type(value, struct addr_entry);
} addr_cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct slice_key);
	__type(value, __u64);
} slices SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16384);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct port_key);
	__type(value, __u64);
} reserved SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct port_key);
	__type(value, __u8);
} svc_ports SEC(".maps");

// The owner of an in-flight fragmented datagram, from its first fragment.
// The only LRU map: entries are short-lived and never deleted; eviction
// under pressure only costs later fragments of old datagrams.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct frag_key);
	__type(value, __u64);
} frags SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, __u64);
	__type(value, struct node);
} nodes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16384);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, struct underlay_key);
	__type(value, __u64);
} node_by_underlay SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, __u32);
	__type(value, struct pool_if);
} pool_ifs SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 256);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, __u32);
	__type(value, struct holders);
} steer SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct egress_config);
} egress_cfg SEC(".maps");

// Sized, not typed: bpf2go can't express bpf_fib_lookup's anonymous
// unions, and only the programs read it.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__uint(value_size, sizeof(struct scratch));
} egress_scratch SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, R_MAX * 2);
	__type(key, __u32);
	__type(value, __u64);
} egress_reasons SEC(".maps");

static __always_inline struct egress_config *config(void)
{
	__u32 z = 0;
	return bpf_map_lookup_elem(&egress_cfg, &z);
}

static __always_inline struct scratch *scr(void)
{
	__u32 z = 0;
	return bpf_map_lookup_elem(&egress_scratch, &z);
}

static __always_inline void count(__u32 reason, __u8 family)
{
	__u32 i = reason * 2 + (family == AF_INET6_ ? 1 : 0);
	__u64 *v = bpf_map_lookup_elem(&egress_reasons, &i);

	if (v)
		*v += 1; // per-CPU: no atomics needed
}

// Write field into the mark byte, the whole 32 bits at once: without the
// barrier clang narrows the masked update into a 1-byte store, and the
// verifier only accepts 4-byte writes to __sk_buff->mark.
static __always_inline void set_mark(struct __sk_buff *skb, struct egress_config *c, __u32 field)
{
	__u32 shift = c->mark_shift & 24;
	__u32 m = (skb->mark & ~(0xffu << shift)) | ((field & 0xffu) << shift);

	asm volatile("" : "+r"(m));
	skb->mark = m;
}

// ---- parsing ---------------------------------------------------------------

static __always_inline int ipv6_ext(__u8 nh)
{
	return nh == IPPROTO_HOPOPTS_ || nh == IPPROTO_ROUTING_ || nh == IPPROTO_FRAGMENT_ ||
	       nh == IPPROTO_AH_ || nh == IPPROTO_DSTOPTS_;
}

static __always_inline int icmp_error(__u8 proto, __u8 type)
{
	if (proto == IPPROTO_ICMP)
		return type == 3 || type == 11 || type == 12;
	return type >= 1 && type <= 4;
}

struct walk_ctx {
	struct __sk_buff *skb;
};

// One IPv6 extension header per call, state in scratch. A bpf_loop callback
// is verified once however many headers it walks.
static long ext_step(__u32 i, void *ctx)
{
	struct walk_ctx *wc = ctx;
	struct scratch *s = scr();
	struct flow *f;
	__u8 *b, nh;
	__u32 fo;

	if (!s)
		return 1;
	f = &s->f;
	b = s->eh;
	nh = s->w_nh;
	if (!ipv6_ext(nh)) {
		s->w_done = 1;
		return 1;
	}
	if (bpf_skb_load_bytes(wc->skb, s->w_off, b, 8) < 0)
		return 1; // w_done stays 0: not parsed
	s->w_nh = b[0];
	if (nh == IPPROTO_FRAGMENT_) {
		fo = (__u32)b[2] << 8 | b[3];
		if (fo & 0xfff8)
			f->frag = 2;
		else if (fo & 1)
			f->frag = 1;
		// Offset 0 without M is an atomic fragment: a whole datagram.
		f->frag_id = (__u32)b[4] << 24 | (__u32)b[5] << 16 | (__u32)b[6] << 8 | b[7];
		f->fproto = b[0];
		s->w_off += 8;
		if (f->frag == 2) {
			s->w_done = 1; // a later fragment has no further headers
			return 1;
		}
		return 0;
	}
	if (nh == IPPROTO_AH_)
		s->w_off += ((__u32)b[1] + 2) * 4;
	else
		s->w_off += ((__u32)b[1] + 1) * 8;
	return 0;
}

// The walk's own frame: bpf_loop takes the skb through a stack context,
// which, inlined, shared a stack slot with a later 4-byte key ("attempt to
// corrupt spilled pointer on stack").
static __noinline int walk_ext(struct __sk_buff *skb)
{
	struct walk_ctx wc = { .skb = skb };
	struct scratch *s = scr();

	if (!s)
		return 0;
	bpf_loop(IPV6_MAX_EXT, ext_step, &wc, 0);
	return s->w_done;
}

// The IP header at l3, into s->hdr and s->f's addresses and protocol.
// Returns -1 when it isn't a readable IPv4 or IPv6 header.
static __always_inline int parse_ip(struct __sk_buff *skb, struct scratch *s, __u32 l3, __be16 proto)
{
	struct flow *f = &s->f;
	__u8 *h = s->hdr;

	__builtin_memset(f, 0, sizeof(*f));
	if (proto == ETH_P_IP_BE) {
		if (bpf_skb_load_bytes(skb, l3, h, 20) < 0)
			return -1;
		if ((h[0] >> 4) != 4 || (h[0] & 0xf) < 5)
			return -1;
		if (((h[6] & 0x1f) | h[7]) != 0)
			f->frag = 2;
		else if (h[6] & 0x20)
			f->frag = 1;
		f->frag_id = (__u32)h[4] << 8 | h[5];
		f->src.family = f->dst.family = AF_INET_;
		__builtin_memcpy(&f->src.addr[0], h + 12, 4);
		__builtin_memcpy(&f->dst.addr[0], h + 16, 4);
		f->proto = f->fproto = h[9];
		f->l4 = l3 + (h[0] & 0xf) * 4;
		return 0;
	}
	if (proto == ETH_P_IPV6_BE) {
		if (bpf_skb_load_bytes(skb, l3, h, 40) < 0)
			return -1;
		if ((h[0] >> 4) != 6)
			return -1;
		f->src.family = f->dst.family = AF_INET6_;
		__builtin_memcpy(f->src.addr, h + 8, 16);
		__builtin_memcpy(f->dst.addr, h + 24, 16);
		f->proto = h[6];
		f->l4 = l3 + 40;
		return 0;
	}
	return -1;
}

// The rest after parse_ip: extension headers, then the ports (TCP, UDP,
// SCTP: the same first four bytes), an echo identifier, or an ICMP error's
// quoted ports. Returns -1 when the packet can't be read that far. A later
// fragment carries none: frag 2, ported 0.
static __always_inline int parse_l4(struct __sk_buff *skb, struct scratch *s)
{
	struct flow *f = &s->f;
	__u8 *b = s->l4b, *q = s->qh, *qp = s->qp;
	__u32 qoff;
	__u8 qproto;

	if (f->dst.family == AF_INET6_ && ipv6_ext(f->proto)) {
		s->w_off = f->l4;
		s->w_nh = f->proto;
		s->w_done = 0;
		if (!walk_ext(skb))
			return -1;
		f->proto = s->w_nh;
		f->l4 = s->w_off;
	}
	if (f->frag == 2)
		return 0;
	if (f->proto != IPPROTO_TCP && f->proto != IPPROTO_UDP && f->proto != IPPROTO_SCTP &&
	    f->proto != IPPROTO_ICMP && f->proto != IPPROTO_ICMPV6_)
		return 0; // another protocol: only a PerPod address covers it
	// One 8-byte load for every protocol: loads of different lengths were
	// merged into one call with a path-dependent destination.
	if (bpf_skb_load_bytes(skb, f->l4, b, 8) < 0)
		return -1;
	if (f->proto == IPPROTO_TCP || f->proto == IPPROTO_UDP || f->proto == IPPROTO_SCTP) {
		f->sport = (__u16)b[0] << 8 | b[1];
		f->dport = (__u16)b[2] << 8 | b[3];
		f->ported = 1;
		return 0;
	}
	if ((f->proto == IPPROTO_ICMP && (b[0] == 0 || b[0] == 8)) ||
	    (f->proto == IPPROTO_ICMPV6_ && (b[0] == 128 || b[0] == 129))) {
		// The echo identifier, which the NAT confined to the slice, is the
		// slice key both ways.
		f->sport = f->dport = (__u16)b[4] << 8 | b[5];
		f->ported = 1;
		return 0;
	}
	if (!icmp_error(f->proto, b[0]))
		return 0;
	// An ICMP error quotes the packet that caused it: one of this address's
	// own, so the quoted source port is the slice's (a reply direction
	// error), or a reply to it, so the quoted destination port is (a transit
	// direction error). The quoted header's options or extension headers
	// aren't walked: such an error finds no owner.
	qoff = f->l4 + 8;
	if (f->dst.family == AF_INET_) {
		if (bpf_skb_load_bytes(skb, qoff, q, 20) < 0)
			return -1;
		qproto = q[9];
		qoff += (q[0] & 0xf) * 4;
	} else {
		if (bpf_skb_load_bytes(skb, qoff, q, 40) < 0)
			return -1;
		qproto = q[6];
		qoff += 40;
	}
	if (bpf_skb_load_bytes(skb, qoff, qp, 8) < 0)
		return -1;
	if (qproto == IPPROTO_TCP || qproto == IPPROTO_UDP || qproto == IPPROTO_SCTP) {
		f->dport = (__u16)qp[0] << 8 | qp[1]; // quoted source: ours
		f->sport = (__u16)qp[2] << 8 | qp[3]; // quoted destination: ours, for transit
		f->ported = 1;
		f->proto = qproto;                    // the slice lookup is by port
	} else if (qproto == IPPROTO_ICMP || qproto == IPPROTO_ICMPV6_) {
		f->sport = f->dport = (__u16)qp[4] << 8 | qp[5];
		f->ported = 1;
	}
	return 0;
}

// ---- owners ----------------------------------------------------------------

// The owner of port on address a, in reserved then slices; 0 when none.
static __always_inline __u64 port_owner(struct scratch *s, const struct addr_key *a,
					const struct addr_entry *c, __u8 proto, __u16 port)
{
	struct port_key *pk = &s->pk;
	struct slice_key *sk = &s->sk;
	__u64 *o;

	__builtin_memset(pk, 0, sizeof(*pk));
	__builtin_memcpy(pk->addr, a->addr, 16);
	pk->family = a->family;
	pk->proto = proto;
	pk->port = port;
	o = bpf_map_lookup_elem(&reserved, pk);
	if (o)
		return *o;
	if (port < c->base)
		return 0;
	__builtin_memset(sk, 0, sizeof(*sk));
	__builtin_memcpy(sk->addr, a->addr, 16);
	sk->family = a->family;
	sk->index = port >> (c->shift & 15);
	o = bpf_map_lookup_elem(&slices, sk);
	return o ? *o : 0;
}

// The owner of the packet on address a by port (the destination port for a
// reply, the source port for transit), as a value, never a pointer into one
// of two maps. A PerPod address has one owner for everything. A first
// fragment records its owner in frags (never the sender, so a forged first
// fragment authorises nothing); a later one takes it, or FRAG_MISS.
static __always_inline __u64 owner_of(struct scratch *s, const struct addr_key *a,
				      const struct addr_entry *c, __u16 port)
{
	struct flow *f = &s->f;
	struct frag_key *k = &s->fk;
	__u64 owner = 0, *o;

	if (c->whole_owner)
		return c->whole_owner;
	if (f->frag != 2) {
		if (f->ported)
			owner = port_owner(s, a, c, f->proto, port);
		if (f->frag == 0 || owner == 0)
			return owner;
	}
	__builtin_memset(k, 0, sizeof(*k));
	__builtin_memcpy(k->src, f->src.addr, 16);
	__builtin_memcpy(k->dst, f->dst.addr, 16);
	k->id = f->frag_id;
	k->family = f->dst.family;
	k->proto = f->fproto;
	if (f->frag == 1) {
		s->fval = owner;
		bpf_map_update_elem(&frags, k, &s->fval, BPF_ANY);
		return owner;
	}
	o = bpf_map_lookup_elem(&frags, k);
	return o ? *o : FRAG_MISS;
}

// ---- tunnel key ------------------------------------------------------------

// Whether the tunnel key toward n is IPv6: the inner family when both
// ends have it, else the one they share.
static __always_inline __u32 key_v6(struct egress_config *c, struct scratch *s, const struct node *n)
{
	return !(s->f.dst.family == AF_INET_ && c->has_v4 && n->has_v4) && c->has_v6 && n->has_v6;
}

// Fill and set the tunnel key toward node n. The outer family is the inner
// family when both ends have it, else the one they share (D3).
// remote_ipv4 and remote_ipv6[0] share one word. Without CAP_PERFMON a
// family branch around the stores was merged by clang into a pointer whose
// offset depended on the path, which the verifier refuses; so every word
// is stored unconditionally, masked by the family (the mask behind a
// barrier, so clang can't rebuild the branch), and the family goes in as
// a scalar flag.
static __always_inline int set_key(struct __sk_buff *skb, struct egress_config *c,
				   struct scratch *s, const struct node *n)
{
	struct bpf_tunnel_key *k = &s->key;
	__u32 v6 = key_v6(c, s, n), mask, w1, w2, w3, w0;

	if (!v6 && !(c->has_v4 && n->has_v4))
		return -1; // no underlay family in common
	mask = 0 - v6;
	asm volatile("" : "+r"(mask));
	w0 = (n->v6[0] & mask) | (bpf_ntohl(n->v4) & ~mask); // IPv4 in host order
	w1 = n->v6[1] & mask;
	w2 = n->v6[2] & mask;
	w3 = n->v6[3] & mask;
	__builtin_memset(k, 0, sizeof(*k));
	k->tunnel_id = GENEVE_VNI;
	k->tunnel_ttl = 64;
	k->remote_ipv6[0] = w0;
	k->remote_ipv6[1] = w1;
	k->remote_ipv6[2] = w2;
	k->remote_ipv6[3] = w3;
	return bpf_skb_set_tunnel_key(skb, k, sizeof(*k), (__u64)(v6 & 1) * BPF_F_TUNINFO_IPV6);
}

// ---- Packet Too Big (design 6.13) ------------------------------------------

static __always_inline __u16 csum_fold_(__s64 c)
{
	__u32 x = (__u32)c;

	x = (x & 0xffff) + (x >> 16);
	x = (x & 0xffff) + (x >> 16);
	return (__u16)~x;
}

// Rewrite the oversize IPv6 reply in skb into an ICMPv6 Packet Too Big from
// the egress address to its sender, quoting PTB6_QUOTE bytes, and send it
// back out the device it arrived on. Every size is a constant: the packet
// is larger than tun_mtu >= 1280.
static __always_inline int ptb6(struct __sk_buff *skb, struct egress_config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	__u8 *ip = s->ip, *ic = s->icmp, *ps = s->ps;
	__u32 mtu = c->tun_mtu, plen = 8 + PTB6_QUOTE;
	__s64 sum;
	__u16 cs;

	__builtin_memset(ip, 0, 40);
	ip[0] = 0x60;
	ip[4] = plen >> 8;
	ip[5] = plen & 0xff;
	ip[6] = IPPROTO_ICMPV6_;
	ip[7] = 64;
	__builtin_memcpy(ip + 8, f->dst.addr, 16);
	__builtin_memcpy(ip + 24, f->src.addr, 16);
	__builtin_memset(ps, 0, 40);
	__builtin_memcpy(ps, ip + 8, 32);
	ps[34] = plen >> 8;
	ps[35] = plen & 0xff;
	ps[39] = IPPROTO_ICMPV6_;
	ic[0] = 2; // Packet Too Big
	ic[1] = 0;
	ic[2] = 0;
	ic[3] = 0;
	ic[4] = mtu >> 24;
	ic[5] = mtu >> 16;
	ic[6] = mtu >> 8;
	ic[7] = mtu;
	sum = bpf_csum_diff(0, 0, (__be32 *)s->q, 512, 0);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)(s->q + 512), 512, sum);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)(s->q + 1024), PTB6_QUOTE - 1024, sum);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)ps, 40, sum);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)ic, 8, sum);
	if (sum < 0)
		goto fail;
	cs = csum_fold_(sum);
	__builtin_memcpy(ic + 2, &cs, 2);
	// Keep the Ethernet header and the quote, then make room in front for
	// the new headers: [eth][ip6][icmp6][quote].
	if (bpf_skb_change_tail(skb, ETH_HLEN + PTB6_QUOTE, 0) < 0 ||
	    bpf_skb_change_head(skb, 48, 0) < 0)
		goto fail;
	if (bpf_skb_store_bytes(skb, 0, s->eth, ETH_HLEN, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN, ip, 40, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN + 40, ic, 8, 0) < 0)
		goto fail;
	count(R_PTB_SENT, AF_INET6_);
	return bpf_redirect(skb->ifindex, 0);
fail:
	count(R_PTB_FAILED, AF_INET6_);
	return TCX_DROP;
}

// The IPv4 counterpart: destination unreachable, fragmentation needed, with
// the next-hop MTU, quoting PTB4_QUOTE bytes (RFC 1812 4.3.2.3).
static __always_inline int ptb4(struct __sk_buff *skb, struct egress_config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	__u8 *ip = s->ip, *ic = s->icmp;
	__u32 mtu = c->tun_mtu, tot = 20 + 8 + PTB4_QUOTE;
	__s64 sum;
	__u16 cs;

	__builtin_memset(ip, 0, 20);
	ip[0] = 0x45;
	ip[2] = tot >> 8;
	ip[3] = tot & 0xff;
	ip[8] = 64;
	ip[9] = IPPROTO_ICMP;
	__builtin_memcpy(ip + 12, &f->dst.addr[0], 4);
	__builtin_memcpy(ip + 16, &f->src.addr[0], 4);
	sum = bpf_csum_diff(0, 0, (__be32 *)ip, 20, 0);
	if (sum < 0)
		goto fail;
	cs = csum_fold_(sum);
	__builtin_memcpy(ip + 10, &cs, 2);
	ic[0] = 3; // destination unreachable
	ic[1] = 4; // fragmentation needed
	ic[2] = 0;
	ic[3] = 0;
	ic[4] = 0;
	ic[5] = 0;
	ic[6] = mtu >> 8;
	ic[7] = mtu;
	sum = bpf_csum_diff(0, 0, (__be32 *)s->q, 512, 0);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)(s->q + 512), PTB4_QUOTE - 512, sum);
	if (sum >= 0)
		sum = bpf_csum_diff(0, 0, (__be32 *)ic, 8, sum);
	if (sum < 0)
		goto fail;
	cs = csum_fold_(sum);
	__builtin_memcpy(ic + 2, &cs, 2);
	if (bpf_skb_change_tail(skb, ETH_HLEN + PTB4_QUOTE, 0) < 0 ||
	    bpf_skb_change_head(skb, 28, 0) < 0)
		goto fail;
	if (bpf_skb_store_bytes(skb, 0, s->eth, ETH_HLEN, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN, ip, 20, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN + 20, ic, 8, 0) < 0)
		goto fail;
	count(R_PTB_SENT, AF_INET_);
	return bpf_redirect(skb->ifindex, 0);
fail:
	count(R_PTB_FAILED, AF_INET_);
	return TCX_DROP;
}

// The PTB is built on an Ethernet uplink's frame: its Ethernet header goes
// back with the MAC addresses swapped. On an IPv4 underlay the outer packet
// is fragmented instead (outer IPv4 leaves without DF), so only an IPv6
// outer is too big. GSO packets are segmented to gso_size later.
static __always_inline int send_ptb(struct __sk_buff *skb, struct egress_config *c, struct scratch *s)
{
	__u8 *e = s->eth, t;
	int i;

	if (bpf_skb_load_bytes(skb, 0, s->eth, ETH_HLEN) < 0 ||
	    bpf_skb_load_bytes(skb, ETH_HLEN, s->q, PTB6_QUOTE) < 0) {
		count(R_PTB_FAILED, s->f.dst.family);
		return TCX_DROP;
	}
	for (i = 0; i < 6; i++) {
		t = e[i];
		e[i] = e[6 + i];
		e[6 + i] = t;
	}
	if (s->f.dst.family == AF_INET6_)
		return ptb6(skb, c, s);
	return ptb4(skb, c, s);
}

// ---- uplink ----------------------------------------------------------------

// pl_announce on an uplink. l3 is where the network header starts: ETH_HLEN
// on Ethernet links (the outer VLAN tag is already in skb metadata; an
// inner one is in-band), 0 on links without a link-layer header.
static __always_inline int uplink(struct __sk_buff *skb, __u32 l3)
{
	struct egress_config *c;
	struct scratch *s;
	struct addr_entry *ac;
	struct flow *f;
	struct node *n;
	struct pool_if *pif;
	__be16 proto = skb->protocol;
	__u32 tags = skb->vlan_present ? 1 : 0;
	__u8 type;

	s = scr();
	c = config();
	if (!s || !c)
		return TCX_NEXT;
	f = &s->f;
	// In-band VLAN tags: [TCI][encapsulated ethertype] each.
#pragma unroll
	for (int i = 0; i < MAX_VLAN_TAGS; i++) {
		if (l3 == 0 || (proto != ETH_P_8021Q_BE && proto != ETH_P_8021AD_BE))
			break;
		if (tags >= MAX_VLAN_TAGS || bpf_skb_load_bytes(skb, l3, s->l4b, 4) < 0)
			return TCX_NEXT; // the guard decides what it can't read
		proto = (__be16)((__u16)s->l4b[3] << 8 | s->l4b[2]);
		l3 += 4;
		tags++;
	}
	if (parse_ip(skb, s, l3, proto) < 0)
		return TCX_NEXT;
	// Neighbour discovery (RS, RA, NS, NA, Redirect) is never ours.
	if (f->dst.family == AF_INET6_ && f->proto == IPPROTO_ICMPV6_) {
		if (bpf_skb_load_bytes(skb, f->l4, &s->l4b, 1) < 0)
			return TCX_NEXT;
		type = s->l4b[0];
		if (type >= 133 && type <= 137)
			return TCX_NEXT;
	}
	ac = bpf_map_lookup_elem(&addr_cfg, &f->dst);
	if (!ac)
		return TCX_NEXT; // the fast path: not an egress address
	if (!ac->held) {
		count(R_NOT_ANNOUNCER, f->dst.family);
		return TCX_DROP;
	}
	if (parse_l4(skb, s) < 0) {
		count(R_MALFORMED, f->dst.family);
		return TCX_DROP;
	}
	if (f->ported && f->frag != 2 && !ac->whole_owner &&
	    (f->proto == IPPROTO_TCP || f->proto == IPPROTO_UDP || f->proto == IPPROTO_SCTP)) {
		struct port_key *pk = &s->pk;

		__builtin_memset(pk, 0, sizeof(*pk));
		__builtin_memcpy(pk->addr, f->dst.addr, 16);
		pk->family = f->dst.family;
		pk->proto = f->proto;
		pk->port = f->dport;
		if (bpf_map_lookup_elem(&svc_ports, pk)) {
			count(R_SERVICE_PORT, f->dst.family);
			return TCX_NEXT;
		}
	}
	s->owner = owner_of(s, &f->dst, ac, f->dport);
	if (s->owner == FRAG_MISS) {
		count(R_FRAG_MISSING, f->dst.family);
		return TCX_DROP;
	}
	if (s->owner == 0) {
		count(R_NO_OWNER, f->dst.family);
		return TCX_DROP;
	}
	if (s->owner == c->self) {
		// The D32 chain admits only established and related packets. The
		// direct-path mark routes the reverse path check through the pool
		// interface's table under strict rp_filter.
		// The key in scratch: on 6.6, two 4-byte stack values sharing a
		// slot are refused ("attempt to corrupt spilled pointer").
		s->ifindex = skb->ifindex;
		pif = bpf_map_lookup_elem(&pool_ifs, &s->ifindex);
		if (pif && pif->n <= DIRECT_MAX)
			set_mark(skb, c, REPLY_FLAG + 1 + pif->n);
		count(R_OWNER_LOCAL, f->dst.family);
		return TCX_NEXT;
	}
	n = bpf_map_lookup_elem(&nodes, &s->owner);
	if (!n) {
		count(R_NO_NODE, f->dst.family);
		return TCX_DROP;
	}
	// The PTB is built on an untagged Ethernet frame (an in-band tag would
	// move the quote).
	if (l3 == ETH_HLEN && key_v6(c, s, n) && c->tun_mtu >= PTB_MIN_MTU &&
	    !skb->gso_size && skb->len > l3 + c->tun_mtu)
		return send_ptb(skb, c, s);
	// Out through the tunnel without the VLAN tags: on L3 Geneve the
	// redirect strips only the Ethernet header.
#pragma unroll
	for (int i = 0; i < MAX_VLAN_TAGS; i++) {
		if (!skb->vlan_present && skb->protocol != ETH_P_8021Q_BE && skb->protocol != ETH_P_8021AD_BE)
			break;
		if (bpf_skb_vlan_pop(skb) < 0) {
			count(R_MALFORMED, f->dst.family);
			return TCX_DROP;
		}
	}
	if (set_key(skb, c, s, n) < 0) {
		count(R_NO_NODE, f->dst.family);
		return TCX_DROP;
	}
	if (l3 && c->l2 && bpf_skb_store_bytes(skb, 0, n->mac, 6, 0) < 0) {
		count(R_MALFORMED, f->dst.family);
		return TCX_DROP;
	}
	count(R_TO_TUNNEL, f->dst.family);
	return bpf_redirect(c->tun_ifindex, 0);
}

SEC("tcx/ingress")
int pl_announce_eth(struct __sk_buff *skb)
{
	return uplink(skb, ETH_HLEN);
}

SEC("tcx/ingress")
int pl_announce_l3(struct __sk_buff *skb)
{
	return uplink(skb, 0);
}

// ---- purelb-eg0 ------------------------------------------------------------

// Decrement the IPv4 TTL or IPv6 hop limit of transit (a forwarding hop),
// through helpers. Returns -1 when it would expire.
static __always_inline int dec_hop(struct __sk_buff *skb, struct scratch *s, __u32 l3)
{
	__u8 *h = s->hdr, v;

	if (s->f.dst.family == AF_INET_) {
		__be16 old, new;

		if (h[8] <= 1)
			return -1;
		old = bpf_htons((__u16)h[8] << 8 | h[9]);
		new = bpf_htons((__u16)(h[8] - 1) << 8 | h[9]);
		v = h[8] - 1;
		if (bpf_l3_csum_replace(skb, l3 + 10, old, new, 2) < 0)
			return -1;
		return bpf_skb_store_bytes(skb, l3 + 8, &v, 1, 0) < 0 ? -1 : 0;
	}
	if (h[7] <= 1)
		return -1;
	v = h[7] - 1;
	return bpf_skb_store_bytes(skb, l3 + 7, &v, 1, 0) < 0 ? -1 : 0;
}

// Send transit out the address's pool interface: a FIB lookup in its table,
// then bpf_redirect_neigh to the next hop it found.
static __always_inline int forward(struct __sk_buff *skb, struct scratch *s, const struct addr_entry *ac)
{
	struct bpf_fib_lookup *fib = &s->fib;
	struct bpf_redir_neigh *nh = &s->nh;
	struct flow *f = &s->f;
	long r;

	__builtin_memset(fib, 0, sizeof(*fib));
	__builtin_memset(nh, 0, sizeof(*nh));
	fib->ifindex = skb->ifindex;
	fib->tbid = ac->table;
	fib->family = f->dst.family;
	fib->l4_protocol = f->proto;
	if (f->dst.family == AF_INET_) {
		fib->ipv4_src = f->src.addr[0];
		fib->ipv4_dst = f->dst.addr[0];
	} else {
		__builtin_memcpy(fib->ipv6_src, f->src.addr, 16);
		__builtin_memcpy(fib->ipv6_dst, f->dst.addr, 16);
	}
	r = bpf_fib_lookup(skb, fib, sizeof(*fib),
			   BPF_FIB_LOOKUP_DIRECT | BPF_FIB_LOOKUP_TBID | BPF_FIB_LOOKUP_SKIP_NEIGH);
	if (r != BPF_FIB_LKUP_RET_SUCCESS) {
		count(R_NO_ROUTE, f->dst.family);
		return TCX_DROP;
	}
	// The lookup leaves the gateway, or the destination itself on-link, in
	// the destination field.
	nh->nh_family = fib->family;
	if (fib->family == AF_INET_)
		nh->ipv4_nh = fib->ipv4_dst;
	else
		__builtin_memcpy(nh->ipv6_nh, fib->ipv6_dst, 16);
	count(R_TRANSIT, f->dst.family);
	return bpf_redirect_neigh(fib->ifindex, nh, sizeof(*nh), 0);
}

SEC("tcx/ingress")
int pl_announce_tun(struct __sk_buff *skb)
{
	struct egress_config *c;
	struct scratch *s;
	struct addr_entry *ac;
	struct flow *f;
	struct underlay_key *uk;
	__u64 *sender;
	__be16 proto = skb->protocol;
	__u32 l3 = 0;

	s = scr();
	c = config();
	if (!s || !c)
		return TCX_DROP;
	f = &s->f;
	uk = &s->uk;
	// Only nodes may send tunnel traffic. The underlay family can differ
	// from the inner one; the helper refuses the wrong family.
	__builtin_memset(uk, 0, sizeof(*uk));
	if (bpf_skb_get_tunnel_key(skb, &s->key, sizeof(s->key), 0) == 0 && s->key.remote_ipv4) {
		uk->addr[0] = bpf_htonl(s->key.remote_ipv4);
	} else if (bpf_skb_get_tunnel_key(skb, &s->key, sizeof(s->key), BPF_F_TUNINFO_IPV6) == 0) {
		uk->addr[0] = s->key.remote_ipv6[0];
		uk->addr[1] = s->key.remote_ipv6[1];
		uk->addr[2] = s->key.remote_ipv6[2];
		uk->addr[3] = s->key.remote_ipv6[3];
		uk->v6 = 1;
	}
	sender = bpf_map_lookup_elem(&node_by_underlay, uk);
	if (!sender) {
		count(R_FORGED_TUNNEL, uk->v6 ? AF_INET6_ : AF_INET_);
		return TCX_DROP;
	}
	s->fval = *sender; // the sender, held across the map lookups below
	if (c->l2) {
		// The relayed frame carries the uplink's MAC: as PACKET_OTHERHOST
		// the stack would drop it.
		if (bpf_skb_load_bytes(skb, 12, s->l4b, 2) < 0 ||
		    bpf_skb_store_bytes(skb, 0, c->tun_mac, 6, 0) < 0)
			return TCX_DROP;
		bpf_skb_change_type(skb, PACKET_HOST);
		proto = (__be16)((__u16)s->l4b[1] << 8 | s->l4b[0]);
		l3 = ETH_HLEN;
	}
	if (parse_ip(skb, s, l3, proto) < 0 || parse_l4(skb, s) < 0) {
		count(R_FORGED_INNER_DST, f->dst.family);
		return TCX_DROP;
	}

	// Transit: the inner source is an egress address this node holds, and
	// its port lies in the sender's slice or reserved port.
	ac = bpf_map_lookup_elem(&addr_cfg, &f->src);
	if (ac && ac->held) {
		__u64 from = s->fval;

		s->owner = owner_of(s, &f->src, ac, f->sport);
		if (s->owner == FRAG_MISS) {
			count(R_FRAG_MISSING, f->dst.family);
			return TCX_DROP;
		}
		if (s->owner == 0 || s->owner != from) {
			count(R_FORGED_INNER, f->dst.family);
			return TCX_DROP;
		}
		if (dec_hop(skb, s, l3) < 0) {
			count(R_TTL_EXPIRED, f->dst.family);
			return TCX_DROP;
		}
		if (!c->l2) {
			// bpf_redirect_neigh drops an skb without an Ethernet header.
			if (bpf_skb_change_head(skb, ETH_HLEN, 0) < 0)
				return TCX_DROP;
			__builtin_memset(s->eth, 0, ETH_HLEN);
			s->eth[12] = (__u8)(proto & 0xff);
			s->eth[13] = (__u8)(proto >> 8);
			if (bpf_skb_store_bytes(skb, 0, s->eth, ETH_HLEN, 0) < 0)
				return TCX_DROP;
		}
		return forward(skb, s, ac);
	}

	// A reply for a pod on this node: the slice must be ours.
	ac = bpf_map_lookup_elem(&addr_cfg, &f->dst);
	if (ac) {
		s->owner = owner_of(s, &f->dst, ac, f->dport);
		if (s->owner == FRAG_MISS) {
			count(R_FRAG_MISSING, f->dst.family);
			return TCX_DROP;
		}
		if (s->owner == c->self) {
			set_mark(skb, c, REPLY_FLAG);
			count(R_REPLY_LOCAL, f->dst.family);
			return TCX_NEXT;
		}
	}
	count(R_FORGED_INNER_DST, f->dst.family);
	return TCX_DROP;
}

// ---- steering ----------------------------------------------------------------

SEC("tcx/egress")
int pl_steer(struct __sk_buff *skb)
{
	struct egress_config *c;
	struct scratch *s;
	struct holders *st;
	struct node *n;
	__u32 field, idx;
	__u8 fam = skb->protocol == ETH_P_IPV6_BE ? AF_INET6_ : AF_INET_;

	s = scr();
	c = config();
	if (!s || !c)
		return TCX_NEXT;
	field = (skb->mark >> (c->mark_shift & 24)) & 0xff;
	// No slot (or a reply pl_announce already keyed): never overwrite a key.
	if (field == 0 || (field & REPLY_FLAG))
		return TCX_NEXT;
	s->ifindex = field; // the key in scratch, as in uplink()
	st = bpf_map_lookup_elem(&steer, &s->ifindex);
	if (!st || st->n == 0 || st->n > MAX_HOLDERS) {
		count(R_NO_HOLDER, fam);
		return TCX_DROP;
	}
	// bpf_get_hash_recalc is the flow hash ip_fragment copies to every
	// fragment, so a datagram's fragments reach the same holder.
	idx = bpf_get_hash_recalc(skb) % st->n;
	if (idx >= MAX_HOLDERS)
		return TCX_DROP;
	s->owner = st->holders[idx];
	n = bpf_map_lookup_elem(&nodes, &s->owner);
	if (!n) {
		count(R_NO_NODE, fam);
		return TCX_DROP;
	}
	s->f.dst.family = fam; // set_key picks the outer family from it
	if (set_key(skb, c, s, n) < 0) {
		count(R_NO_NODE, fam);
		return TCX_DROP;
	}
	// The outer route lookup uses skb->mark: cleared, it can't match the
	// slot rule (the underlay throw routes are the backstop).
	set_mark(skb, c, 0);
	count(R_STEERED, fam);
	return TCX_NEXT;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
