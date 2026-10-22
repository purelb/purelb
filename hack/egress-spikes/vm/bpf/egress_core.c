// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2026 Acnodal Inc.
//
// Phase 0 spike prototypes for PureLB egress -- NOT production code.
// They exist to prove, on every kernel in the matrix and under the agent's
// exact capabilities (CAP_BPF + CAP_NET_ADMIN, no CAP_PERFMON), that the
// decided datapath works:
//
//   pl_steer      tcx egress on purelb-eg0: slot mark -> Geneve tunnel key
//                 (per-flow hash over the announcer set), mark field cleared
//                 so the outer route lookup cannot loop (decision D3).
//   pl_announce   tcx ingress on an uplink: return a reply to the node that
//                 owns its port slice, through the tunnel (D3, D32).
//   pl_tun_in     tcx ingress on purelb-eg0: transit out the pool interface,
//                 or mark a reply for a local pod; drops forged transit and
//                 replies for slices this node does not own (blocker 8).
//   pl_pod_tag    tcx ingress on a host-side pod device: pod-source check and
//                 the slot mark (D2, D20).
//   pl_announce_all  the design's single pl_announce (6.5): the pl_tun_in body
//                 when skb->ifindex is the configured tunnel, the pl_announce
//                 body otherwise. Same code as the two programs above.
//
// Packet Too Big (design 6.12, user decision): when a reply pl_announce would
// tunnel to its owner over an IPv6 underlay is larger than the tunnel MTU
// (config tun_mtu), the underlay would drop it (IPv6 does not fragment in
// flight). pl_announce instead turns the packet into an ICMPv6 Packet Too Big
// (ICMP frag-needed for IPv4) to its sender, quoting the start of it, and
// sends it back out the interface it arrived on.
//
// IPv6 extension headers (design 6.13): parse() walks Hop-by-Hop, Routing,
// Destination Options, Fragment and AH headers, at most IPV6_MAX_EXT of them,
// in a bpf_loop callback.
//
// Fragments (design 6.5): only a datagram's first fragment carries the L4
// header the slice owner comes from. pl_announce and pl_tun_in record the
// owner they found for a first fragment in the frags LRU map, keyed by
// (source, destination, protocol, IP ID; IPv6: the 32-bit Fragment Header
// ID), and apply it to the later fragments. A later fragment with no entry
// (it arrived before its first fragment) is dropped and counted: the spike
// does not buffer.
//
// Every buffer a helper writes lives in the per-CPU scratch map, never on
// the stack: without CAP_PERFMON, Linux 6.17, 6.18 < 6.18.14 and 6.19 < 6.19.4
// reject a helper call that writes stack bytes on a speculative path (the
// address guard's ag_scratch rule).
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
#define SLOT_MASK   0x7fu
#define AF_INET_    2
#define AF_INET6_   10
#define MAX_PEERS   8
#define MAX_NODES   256
#define GENEVE_VNI  1
#define IPPROTO_ICMPV6_ 58
#define IPPROTO_FRAGMENT_ 44
#define FRAG_MISS   0xffffffffu // owner_of(): a later fragment with no frags entry
#define IPV6_MAX_EXT 8          // extension headers walked, Fragment included
#define PTB6_QUOTE  1232        // 1280 - 40 - 8: an ICMPv6 error fits the IPv6 minimum MTU
#define PTB4_QUOTE  548         // 576 - 20 - 8 (RFC 1812 4.3.2.3)
#define PTB_MIN_MTU 1280        // below this the PTB code is not used (quotes are fixed)

// ICMP and ICMPv6 echo share this layout (linux/icmp.h pulls in glibc
// headers that do not build for the BPF target).
struct icmp_echo {
	__u8 type;
	__u8 code;
	__be16 csum;
	__be16 id;
	__be16 seq;
};

struct peer {
	__u32 addr[4]; // network order; IPv4 in addr[0]
	__u8 v6;
	__u8 pad[3];
};

struct steer {
	__u32 n;
	struct peer peers[MAX_PEERS];
};

struct addr_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad[3];
};

struct addr_cfg {
	__u16 base;      // first slice port
	__u8 shift;      // log2(slice size)
	__u8 announced;  // this node holds the address
};

struct slice_key {
	__u32 addr[4];
	__u8 family;
	__u8 pad;
	__u16 index;     // port >> shift (slices are aligned)
};

struct pod_dev {
	__u32 slot;      // spike: the slot directly (production: pod-set id, D24)
	__u32 v4;        // network order
	__u32 v6[4];
};

struct config {
	__u32 self_node;
	__u32 tun_ifindex;
	__u32 pool_ifindex;
	__u32 l2_mode;   // 1: L2 Geneve (no innerprotoinherit)
	__u32 push_eth;  // L3 mode transit: add an Ethernet header before
	                 // bpf_redirect_neigh (spike measures whether needed)
	__u8 tun_mac[6];
	__u8 pad[2];
	__u32 tun_mtu;   // largest inner packet the tunnel carries; 0: no PTB
};

struct flow {
	struct addr_key dst;
	struct addr_key src;
	__u16 sport;     // source port or echo id: picks the slice for transit
	__u16 dport;     // destination port or echo id: picks the slice for replies
	__u8 proto;      // IPv6: the upper-layer protocol, after the extension headers
	__u8 ok;
	__u8 frag;       // 0: not a fragment, 1: first fragment, 2: later fragment
	__u8 fproto;     // frags key protocol: IPv4 protocol, IPv6 Fragment Header next header
	__u32 frag_id;   // IPv4 IP ID or IPv6 Fragment Header ID (host order)
};

// frags key: what every fragment of one datagram shares.
struct frag_key {
	__u32 src[4];
	__u32 dst[4];
	__u32 id;        // IPv4: the 16-bit IP ID; IPv6: the 32-bit Fragment Header ID
	__u8 family;
	__u8 proto;
	__u8 pad[2];
};

// Without CAP_PERFMON the verifier also refuses partial writes into stack
// slots that held a spilled pointer ("attempt to corrupt spilled pointer on
// stack"), which a parse struct on the stack triggers. Everything a program
// builds field by field lives here instead.
struct scratch {
	struct bpf_tunnel_key key;
	struct flow f;
	struct slice_key sk;
	struct peer outer;
	struct frag_key fk;
	__u32 fval;      // value written into frags
	__u32 owner;     // owner node id, the key for the nodes lookup
	__u8 hdr[40];    // IPv4 or IPv6 header, copied with bpf_skb_load_bytes
	__u8 l4[8];      // ports, or the ICMP echo header
	__u8 fh[8];      // IPv6 extension header being walked
	// IPv6 extension header walk (ext_step), across bpf_loop iterations.
	__u32 w_off;     // offset of the header being walked
	__u8 w_nh;       // its type
	__u8 w_done;     // reached a non-extension header, or a later fragment
	__u8 w_pad[2];
	// Packet Too Big synthesis.
	__u8 eth[16];    // the arriving frame's Ethernet header (14 bytes used)
	__u8 ip[40];     // the new IPv4 (20) or IPv6 (40) header
	__u8 icmp[8];    // the new ICMP / ICMPv6 header
	__u8 ps[40];     // ICMPv6 pseudo header
	__u8 q[PTB6_QUOTE]; // the quoted start of the oversize packet
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
	// Fragments, per direction: transit is pl_tun_in toward the server;
	// reply is pl_announce on the uplink and pl_tun_in for a local pod.
	ST_FRAG_TRANSIT_FIRST,    // first fragment: owner recorded in frags
	ST_FRAG_TRANSIT_FOLLOWED, // later fragment: owner taken from frags
	ST_FRAG_TRANSIT_MISSING,  // later fragment with no entry: dropped
	ST_FRAG_REPLY_FIRST,
	ST_FRAG_REPLY_FOLLOWED,
	ST_FRAG_REPLY_MISSING,
	ST_PTB4_SENT,             // 21: oversize IPv4 reply answered with frag-needed
	ST_PTB6_SENT,             // 22: oversize IPv6 reply answered with Packet Too Big
	ST_PTB_FAILED,            // 23: a helper failed while building one (dropped)
	ST_MAX,
};

#define DIR_TRANSIT 0
#define DIR_REPLY   (ST_FRAG_REPLY_FIRST - ST_FRAG_TRANSIT_FIRST)

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 128);
	__type(key, __u32);
	__type(value, struct steer);
} steer_slots SEC(".maps");

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

// Owner of an in-flight fragmented datagram, filled by its first fragment.
// The only LRU map (design 6.5): entries are short-lived and never deleted;
// eviction under pressure only costs later fragments of old datagrams.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct frag_key);
	__type(value, __u32);
} frags SEC(".maps");

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
	__type(value, struct pod_dev);
} pod_devs SEC(".maps");

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

// Write the whole 32-bit mark. Without the barrier clang narrows a masked
// update into a 1-byte store, and the verifier only accepts 4-byte writes to
// __sk_buff->mark.
static __always_inline void set_mark(struct __sk_buff *skb, __u32 field)
{
	__u32 m = (skb->mark & ~MARK_FIELD) | (field & MARK_FIELD);

	asm volatile("" : "+r"(m));
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

// Fill and set the tunnel key toward p, in the caller's scratch.
//
// remote_ipv4 and remote_ipv6[0] share one word of bpf_tunnel_key, so both
// families store a scalar there unconditionally and only IPv6 writes the
// other three words. Branch-dependent stores or calls were merged by clang
// into one pointer computation whose offset depended on the path, which the
// verifier refuses without CAP_PERFMON ("R0 tried to add from different
// maps, paths or scalars").
static __always_inline int set_key(struct __sk_buff *skb, struct scratch *s, const struct peer *p)
{
	struct bpf_tunnel_key *k = &s->key;
	__u32 w0 = p->addr[0];
	__u64 flags = 0;

	__builtin_memset(k, 0, sizeof(*k));
	k->tunnel_id = GENEVE_VNI;
	k->tunnel_ttl = 64;
	if (!p->v6)
		w0 = bpf_ntohl(w0); // the helper wants IPv4 in host order
	k->remote_ipv6[0] = w0;
	if (p->v6) {
		k->remote_ipv6[1] = p->addr[1];
		k->remote_ipv6[2] = p->addr[2];
		k->remote_ipv6[3] = p->addr[3];
		flags = BPF_F_TUNINFO_IPV6;
	}
	return bpf_skb_set_tunnel_key(skb, k, sizeof(*k), flags);
}

// ---- pl_steer: tcx egress on purelb-eg0 --------------------------------

SEC("tcx/egress")
int pl_steer(struct __sk_buff *skb)
{
	__u32 field = skb->mark & MARK_FIELD;
	__u32 slot = field & SLOT_MASK;
	struct scratch *s;
	struct steer *st;
	__u32 idx;

	// No slot: a relayed reply from pl_announce, which already carries its
	// key. Never overwrite it.
	if (!slot || (field & REPLY_FLAG))
		return TCX_NEXT;
	st = bpf_map_lookup_elem(&steer_slots, &slot);
	if (!st || st->n == 0 || st->n > MAX_PEERS) {
		count(ST_STEER_NO_ANNOUNCER);
		return TCX_DROP; // FailClosed: no announcer for this slot
	}
	// bpf_get_hash_recalc is the flow hash ip_fragment copies to every
	// fragment, so all fragments of a datagram reach the same announcer.
	idx = bpf_get_hash_recalc(skb) % st->n;
	if (idx >= MAX_PEERS)
		return TCX_DROP;
	s = scr();
	if (!s || set_key(skb, s, &st->peers[idx]) < 0)
		return TCX_DROP;
	// The Geneve outer route lookup uses skb->mark; clear the field or the
	// outer packet would match the slot rule and loop back into purelb-eg0.
	set_mark(skb, 0);
	count(ST_STEERED);
	return TCX_NEXT;
}

// ---- shared L3/L4 parsing -------------------------------------------------


// ---- IPv6 extension headers ------------------------------------------------

#define NEXTHDR_HOP_     0
#define NEXTHDR_ROUTING_ 43
#define NEXTHDR_AUTH_    51
#define NEXTHDR_DEST_    60

static __always_inline int ipv6_ext(__u8 nh)
{
	return nh == NEXTHDR_HOP_ || nh == NEXTHDR_ROUTING_ || nh == IPPROTO_FRAGMENT_ ||
	       nh == NEXTHDR_AUTH_ || nh == NEXTHDR_DEST_;
}

struct walk_ctx {
	struct __sk_buff *skb;
};

// One extension header per call, state in scratch (w_off, w_nh, w_done; the
// Fragment Header's fields go straight into s->f). A bpf_loop callback is
// verified once, however many headers it walks: an unrolled walk with an
// exit per header would carry the rest of the program once per exit
// (spike 2 rule 8).
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
	b = s->fh;
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
		// offset 0 without M is an atomic fragment: a whole datagram
		f->frag_id = (__u32)b[4] << 24 | (__u32)b[5] << 16 | (__u32)b[6] << 8 | b[7];
		f->fproto = b[0];
		s->w_off += 8;
		if (f->frag == 2) {
			s->w_done = 1; // no further headers in a later fragment
			return 1;
		}
		return 0;
	}
	if (nh == NEXTHDR_AUTH_)
		s->w_off += ((__u32)b[1] + 2) * 4;
	else
		s->w_off += ((__u32)b[1] + 1) * 8;
	return 0;
}

// The walk's own subprogram: bpf_loop takes the skb through a context on
// the stack, and inlined into the programs clang gave that stack slot (a
// spilled pointer) to count()'s 4-byte key later, which the verifier
// refuses without CAP_PERFMON ("attempt to corrupt spilled pointer on
// stack"; spike 2 rule 2). A separate frame cannot share it.
static __noinline int walk_ext(struct __sk_buff *skb)
{
	struct walk_ctx wc = { .skb = skb };
	struct scratch *s = scr();

	if (!s)
		return 0;
	bpf_loop(IPV6_MAX_EXT, ext_step, &wc, 0);
	return s->w_done;
}

// Parse an IPv4/IPv6 packet at offset l3, recording both ports: transit is
// judged by its source port, a reply by its destination port.
//
// Fragments: f->frag says whether this is a first (offset 0, more set) or a
// later fragment, f->frag_id holds the datagram's ID. A later fragment has
// no L4 header: it is returned with ok = 1 and no ports, and owner_of()
// finds its owner in frags. IPv6: extension headers, the Fragment Header
// among them, are walked by ext_step (at most IPV6_MAX_EXT); a packet with
// more is not parsed (-1).
//
// Everything at a variable offset is copied with bpf_skb_load_bytes into
// scratch. Without CAP_PERFMON the verifier's speculation sanitizer only
// allows variable offsets on stack and map-value pointers, so direct packet
// access with a variable offset ("data + l4") is refused outright -- the same
// reason the address guard copies its headers.
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
			f->frag = 2;      // fragment offset != 0
		else if (h[6] & 0x20)
			f->frag = 1;      // offset 0, more fragments
		f->frag_id = (__u32)h[4] << 8 | h[5];
		f->dst.family = AF_INET_;
		__builtin_memcpy(&f->dst.addr[0], h + 16, 4);
		f->src.family = AF_INET_;
		__builtin_memcpy(&f->src.addr[0], h + 12, 4);
		f->proto = h[9];
		f->fproto = f->proto;
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
		if (ipv6_ext(f->proto)) {
			s->w_off = l4;
			s->w_nh = f->proto;
			s->w_done = 0;
			if (!walk_ext(skb))
				return -1; // more extension headers than walked, or a load failed
			f->proto = s->w_nh;
			l4 = s->w_off;
		}
	} else {
		return -1;
	}

	if (f->frag == 2) {
		// No L4 header: the owner comes from frags (owner_of). For IPv6 the
		// header after the Fragment Header may be another extension header
		// (Destination Options), which only the first fragment walks past.
		if (f->fproto == IPPROTO_TCP || f->fproto == IPPROTO_UDP ||
		    f->fproto == IPPROTO_ICMP || f->fproto == IPPROTO_ICMPV6_ ||
		    (f->dst.family == AF_INET6_ && ipv6_ext(f->fproto)))
			f->ok = 1;
		return 0;
	}
	if (f->proto != IPPROTO_TCP && f->proto != IPPROTO_UDP &&
	    f->proto != IPPROTO_ICMP && f->proto != IPPROTO_ICMPV6_)
		return 0;
	// One 8-byte load for every protocol (UDP and TCP headers are at least
	// 8 bytes): two loads of different lengths were merged by clang into
	// one call with a path-dependent destination, which the verifier
	// refuses without CAP_PERFMON.
	if (bpf_skb_load_bytes(skb, l4, s->l4, 8) < 0)
		return -1;
	if (f->proto == IPPROTO_TCP || f->proto == IPPROTO_UDP) {
		f->sport = (__u16)s->l4[0] << 8 | s->l4[1];
		f->dport = (__u16)s->l4[2] << 8 | s->l4[3];
		f->ok = 1;
	} else {
		__u8 type = s->l4[0];

		// Echo reply (0 / 129) and request (8 / 128) carry the identifier
		// the NAT confined to the slice.
		if ((f->proto == IPPROTO_ICMP && (type == 0 || type == 8)) ||
		    (f->proto == IPPROTO_ICMPV6_ && (type == 129 || type == 128))) {
			f->sport = (__u16)s->l4[4] << 8 | s->l4[5];
			f->dport = f->sport; // the echo id is the slice key both ways
			f->ok = 1;
		} else if (f->proto == IPPROTO_ICMPV6_ && type >= 133 && type <= 137) {
			f->ok = 2; // neighbour discovery: always to the stack
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
	// Slices are power-of-two sized and aligned on absolute port numbers
	// (20480-24575 is 5 << 12), so the index is port >> shift. base only
	// excludes the low ports.
	k->index = port >> c->shift;
	return bpf_map_lookup_elem(&slices, k);
}

// The slice owner of this packet (0: none). port is the source port for
// transit and the destination port for replies; dir is DIR_TRANSIT or
// DIR_REPLY and only picks the counters.
//
// A first fragment looks its owner up by port as any packet does, and
// records it in frags. A later fragment carries no port: it takes the
// recorded owner, or FRAG_MISS when there is none. Returning the owner as a
// scalar, rather than a pointer into slices or frags, keeps the callers off
// a choice between two map values (verifier rule 7).
static __always_inline __u32 owner_of(struct scratch *s, const struct addr_key *a,
				      const struct addr_cfg *c, __u16 port, __u32 dir)
{
	struct flow *f = &s->f;
	struct frag_key *k = &s->fk;
	__u32 *o, owner = 0;

	if (f->frag != 2) {
		o = slice_owner(s, a, c, port);
		if (o)
			owner = *o;
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
		count(ST_FRAG_TRANSIT_FIRST + dir);
		return owner;
	}
	o = bpf_map_lookup_elem(&frags, k);
	if (!o) {
		count(ST_FRAG_TRANSIT_MISSING + dir);
		return FRAG_MISS; // a later fragment before its first: no buffering
	}
	count(ST_FRAG_TRANSIT_FOLLOWED + dir);
	return *o;
}

// Decrement the IPv4 TTL or the IPv6 hop limit, through helpers (l3 is a
// runtime value: 0 for L3 Geneve, ETH_HLEN for L2). Returns -1 when it
// would expire: transit is a forwarding hop. s->hdr holds the header parse()
// copied.
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

// ---- Packet Too Big from pl_announce (design 6.12) ------------------------

static __always_inline __u16 csum_fold_(__s64 c)
{
	__u32 x = (__u32)c;

	x = (x & 0xffff) + (x >> 16);
	x = (x & 0xffff) + (x >> 16);
	return (__u16)~x;
}

// The new frame goes back to the previous hop: swap the MAC addresses of
// the arriving frame's header (s->eth).
static __always_inline void swap_macs(struct scratch *s)
{
	__u8 *e = s->eth, t;
	int i;

	for (i = 0; i < 6; i++) {
		t = e[i];
		e[i] = e[6 + i];
		e[6 + i] = t;
	}
}

// Rewrite the oversize IPv6 packet in skb (s->f parsed, s->q holding its
// first PTB6_QUOTE bytes) into an ICMPv6 Packet Too Big from the
// destination (the egress address) to the source, and send it back out the
// device it arrived on. RFC 4443: quote as much of the packet as fits the
// minimum MTU. The packet is larger than tun_mtu >= 1280, so the quote is
// always exactly PTB6_QUOTE bytes: every size here is a constant.
static __always_inline int ptb6(struct __sk_buff *skb, struct config *c, struct scratch *s)
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
	// the new IPv6 and ICMPv6 headers: [eth][ip6][icmp6][quote].
	if (bpf_skb_change_tail(skb, ETH_HLEN + PTB6_QUOTE, 0) < 0 ||
	    bpf_skb_change_head(skb, 48, 0) < 0)
		goto fail;
	if (bpf_skb_store_bytes(skb, 0, s->eth, ETH_HLEN, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN, ip, 40, 0) < 0 ||
	    bpf_skb_store_bytes(skb, ETH_HLEN + 40, ic, 8, 0) < 0)
		goto fail;
	count(ST_PTB6_SENT);
	return bpf_redirect(skb->ifindex, 0);
fail:
	count(ST_PTB_FAILED);
	return TCX_DROP;
}

// The IPv4 counterpart: ICMP destination unreachable, fragmentation needed
// (type 3 code 4) with the next-hop MTU, quoting PTB4_QUOTE bytes (RFC 1812
// 4.3.2.3: an ICMP error stays within 576 bytes).
static __always_inline int ptb4(struct __sk_buff *skb, struct config *c, struct scratch *s)
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
	count(ST_PTB4_SENT);
	return bpf_redirect(skb->ifindex, 0);
fail:
	count(ST_PTB_FAILED);
	return TCX_DROP;
}

// A reply bound for an IPv6 underlay that does not fit the tunnel: answer
// it instead of tunnelling it (the underlay would drop it, Ip6FragFails).
// GSO packets are left alone (they are segmented to gso_size later).
static __always_inline int too_big(struct __sk_buff *skb, struct config *c, const struct peer *p)
{
	return p->v6 && c->tun_mtu >= PTB_MIN_MTU && !skb->gso_size &&
	       skb->len > ETH_HLEN + c->tun_mtu;
}

static __always_inline int send_ptb(struct __sk_buff *skb, struct config *c, struct scratch *s)
{
	if (bpf_skb_load_bytes(skb, 0, s->eth, ETH_HLEN) < 0 ||
	    bpf_skb_load_bytes(skb, ETH_HLEN, s->q, PTB6_QUOTE) < 0) {
		count(ST_PTB_FAILED);
		return TCX_DROP;
	}
	swap_macs(s);
	if (s->f.dst.family == AF_INET6_)
		return ptb6(skb, c, s);
	return ptb4(skb, c, s);
}

// ---- pl_announce: tcx ingress on an L2 uplink -----------------------------

static __always_inline int announce(struct __sk_buff *skb, struct config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	struct addr_cfg *ac;
	struct peer *p;
	__u32 owner;

	if (parse(skb, s, ETH_HLEN, skb->protocol) < 0)
		return TCX_NEXT;
	ac = bpf_map_lookup_elem(&egress_addrs, &f->dst);
	if (!ac) // fast path: not an egress address
		return TCX_NEXT;
	if (f->ok != 1)
		return TCX_NEXT; // ND, other protocols: stack (nft -150 chain decides)
	owner = owner_of(s, &f->dst, ac, f->dport, DIR_REPLY);
	if (owner == FRAG_MISS)
		return TCX_DROP; // counted ST_FRAG_REPLY_MISSING
	if (owner == 0) {
		count(ST_NO_OWNER);
		return TCX_DROP;
	}
	if (owner == c->self_node) {
		count(ST_OWNER_LOCAL);
		return TCX_NEXT; // conntrack on this node de-NATs it
	}
	s->owner = owner;
	p = bpf_map_lookup_elem(&nodes, &s->owner);
	if (!p)
		return TCX_DROP;
	if (too_big(skb, c, p))
		return send_ptb(skb, c, s);
	if (set_key(skb, s, p) < 0)
		return TCX_DROP;
	count(ST_TO_TUNNEL);
	// L3 Geneve: the redirect drops the Ethernet header (ARPHRD_NONE).
	// L2 Geneve: the frame keeps its header; pl_tun_in on the owner fixes
	// the destination MAC and pkt_type.
	return bpf_redirect(c->tun_ifindex, 0);
}

SEC("tcx/ingress")
int pl_announce(struct __sk_buff *skb)
{
	struct config *c = config();
	struct scratch *s = scr();

	if (!c || !s)
		return TCX_NEXT;
	return announce(skb, c, s);
}

// ---- pl_tun_in: tcx ingress on purelb-eg0 ---------------------------------

static __always_inline int tun_in(struct __sk_buff *skb, struct config *c, struct scratch *s)
{
	struct flow *f = &s->f;
	struct addr_cfg *ac;
	__u32 l3 = 0, owner, *sender;
	__u16 proto = skb->protocol;

	if (c->l2_mode) {
		void *data = (void *)(long)skb->data;
		void *end = (void *)(long)skb->data_end;
		struct ethhdr *eth = data;

		if ((void *)(eth + 1) > end)
			return TCX_DROP;
		proto = eth->h_proto;
		__builtin_memcpy(eth->h_dest, c->tun_mac, 6);
		// The relayed frame carries the uplink's MAC, so the stack would
		// drop it as PACKET_OTHERHOST.
		bpf_skb_change_type(skb, PACKET_HOST);
		l3 = ETH_HLEN;
	}
	if (skb->pkt_type == PACKET_HOST)
		count(ST_PKT_TYPE_HOST);
	else
		count(ST_PKT_TYPE_OTHER);

	if (parse(skb, s, l3, proto) < 0)
		return TCX_NEXT;

	// Transit: the inner source is an egress address this node announces.
	ac = bpf_map_lookup_elem(&egress_addrs, &f->src);
	if (ac && ac->announced) {
		struct peer *outer = &s->outer;

		if (f->ok != 1)
			return TCX_DROP;
		__builtin_memset(outer, 0, sizeof(*outer));
		// The underlay family can differ from the inner family; the helper
		// refuses a mismatched family, so try IPv4 then IPv6.
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
			return TCX_DROP; // outer source is not a cluster node
		}
		// A later fragment is held to the owner its first fragment had:
		// the frags entry carries the slice owner, never the sender.
		owner = owner_of(s, &f->src, ac, f->sport, DIR_TRANSIT);
		if (owner == FRAG_MISS)
			return TCX_DROP; // counted ST_FRAG_TRANSIT_MISSING
		if (owner == 0 || owner != *sender) {
			count(ST_TRANSIT_FORGED);
			return TCX_DROP; // source port outside the sender's slice
		}
		if (dec_hop(skb, s, l3, proto) < 0) {
			count(ST_TRANSIT_TTL);
			return TCX_DROP; // D13: TTL expiry is dropped and counted
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

	// A reply for a pod on this node: the destination slice must be ours.
	ac = bpf_map_lookup_elem(&egress_addrs, &f->dst);
	if (ac && f->ok == 1) {
		owner = owner_of(s, &f->dst, ac, f->dport, DIR_REPLY);
		if (owner == FRAG_MISS)
			return TCX_DROP; // counted ST_FRAG_REPLY_MISSING
		if (owner != c->self_node) {
			count(ST_REPLY_NOT_OWNER);
			return TCX_DROP; // forged or stale: never punt to the stack
		}
		set_mark(skb, REPLY_FLAG);
		count(ST_REPLY_LOCAL);
		return TCX_NEXT;
	}
	return TCX_DROP;
}

SEC("tcx/ingress")
int pl_tun_in(struct __sk_buff *skb)
{
	struct config *c = config();
	struct scratch *s = scr();

	if (!c || !s)
		return TCX_NEXT;
	return tun_in(skb, c, s);
}

// ---- pl_announce_all: pl_announce for both device kinds (design 6.5) ------

// One program on uplinks and on purelb-eg0: the role follows the device.
SEC("tcx/ingress")
int pl_announce_all(struct __sk_buff *skb)
{
	struct config *c = config();
	struct scratch *s = scr();

	if (!c || !s)
		return TCX_NEXT;
	if (c->tun_ifindex && skb->ifindex == c->tun_ifindex)
		return tun_in(skb, c, s);
	return announce(skb, c, s);
}

// ---- pl_pod_tag: tcx ingress on a host-side pod device -------------------

SEC("tcx/ingress")
int pl_pod_tag(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *end = (void *)(long)skb->data_end;
	__u32 ifx = skb->ifindex;
	struct pod_dev *pd = bpf_map_lookup_elem(&pod_devs, &ifx);

	if (!pd)
		return TCX_NEXT; // not a managed pod device
	if (skb->protocol == bpf_htons(ETH_P_IP)) {
		struct iphdr *ip = data + ETH_HLEN;

		if ((void *)(ip + 1) > end)
			return TCX_NEXT;
		if (ip->saddr != pd->v4) {
			count(ST_POD_SPOOF);
			return TCX_DROP;
		}
	} else if (skb->protocol == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6 = data + ETH_HLEN;
		__u32 *sa;

		if ((void *)(ip6 + 1) > end)
			return TCX_NEXT;
		sa = (__u32 *)&ip6->saddr;
		// Link-local (fe80::/10) and the unspecified address (DAD) are the
		// pod's own ND traffic.
		if ((bpf_ntohl(sa[0]) & 0xffc00000) != 0xfe800000 &&
		    (sa[0] | sa[1] | sa[2] | sa[3]) != 0 &&
		    (sa[0] != pd->v6[0] || sa[1] != pd->v6[1] || sa[2] != pd->v6[2] || sa[3] != pd->v6[3])) {
			count(ST_POD_SPOOF);
			return TCX_DROP;
		}
	}
	set_mark(skb, pd->slot & SLOT_MASK);
	count(ST_POD_TAGGED);
	return TCX_NEXT;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
