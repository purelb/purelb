// Copyright 2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The eBPF pod-node stage (design 6.4), included by egress.c. It runs where
// the CNI's datapath keeps a pod's traffic from netfilter, and NATs it in
// PureLB's own map (D32).
//
// pl_pod_out sees a pod's packet at the tcx egress of a host NIC, ahead of the
// CNI. A packet not from a selected pod's own address passes: that covers
// what the node sends, tunnel outer headers, and a translated packet coming
// round again, since bpf_redirect_neigh re-enters this hook (measured). A
// flow's first packet is classified as design 6.3's nft chains do. Then the
// packet goes direct (bpf_redirect_neigh, in the slot's table) or into the
// tunnel (its slot in the mark and bpf_redirect, for pl_steer).
//
// pod_reply is called by pl_announce for a packet to this node's slice: the
// reverse translation, then bpf_redirect_peer into the pod, past the CNI.
//
// What isn't a new flow (user decision 2026-11-04: bpf_redirect_peer takes
// traffic into a pod past every host-side hook, so the design's flow map on
// the host device can't see it): TCP starts a flow only with a SYN without
// ACK, and anything else with no entry passes; ICMP echo replies and errors
// pass; for everything else, pl_pod_inbound, on the root cgroup, records what
// is delivered to a selected pod's socket however it got there, and a pod's
// reply to it passes.
//
// Verifier rules as egress.c's: keys and buffers in scratch, values chosen
// and not addresses, loops in bpf_loop.

#define NAT_PROBES  64
#define POD_POLICIES 255
#define TCP_FIN 0x01
#define TCP_SYN 0x02
#define TCP_RST 0x04
#define TCP_ACK 0x10
#define MSS_OPTIONS 10
#define INBOUND_TTL_NS (180ull * 1000000000ull)

static __always_inline __u32 csum_add(__u32 a, __u32 b)
{
	__u32 r = a + b;

	return r + (r < a);
}

// The kernel's csum_replace_by_diff on a raw 16-bit checksum, for the ones
// inside an ICMP error's quote, which no helper reaches.
static __always_inline __u16 csum_apply(__u16 check, __u32 diff)
{
	__u32 x = csum_add(diff, ~(__u32)check);

	x = (x & 0xffff) + (x >> 16);
	x = (x & 0xffff) + (x >> 16);
	return (__u16)~x;
}

// A value chosen by c, never a choice between two addresses (design 6.6).
static __always_inline __u32 pick(int c, __u32 a, __u32 b)
{
	__u32 m = -(__u32)!!c;

	asm volatile("" : "+r"(a), "+r"(b));
	return (a & m) | (b & ~m);
}

static __always_inline void nat_key_of(struct nat_key *k, const __u32 *r, const __u32 *l,
				       __u16 rport, __u16 lport, __u8 proto, __u8 family, __u8 dir)
{
	__builtin_memset(k, 0, sizeof(*k));
	__builtin_memcpy(k->raddr, r, 16);
	__builtin_memcpy(k->laddr, l, 16);
	k->rport = rport;
	k->lport = lport;
	k->proto = proto;
	k->family = family;
	k->dir = dir;
}

static __always_inline __u8 nat_state(__u8 st, __u8 proto, __u8 fl, int reply)
{
	if (proto == IPPROTO_TCP) {
		if (fl & TCP_RST)
			return NS_CLOSED;
		if (fl & TCP_FIN)
			return st == NS_CLOSED ? st : NS_FIN;
		if (!reply && (fl & TCP_SYN) && !(fl & TCP_ACK))
			return NS_NEW; // the same tuple opened again
	}
	if (reply && st == NS_NEW)
		return NS_EST;
	return st;
}

static __always_inline int is_icmp(__u8 p)
{
	return p == IPPROTO_ICMP || p == IPPROTO_ICMPV6_;
}

// Rewrite the source (dst 0) or destination (dst 1) address to s->n and the
// port or echo identifier to nport, with the IPv4 header checksum and the
// L4 checksum. A later fragment has no L4 header; SCTP (PerPod only) keeps
// its port and its CRC32c, which covers no address; another protocol is the
// address alone.
//
// Under CHECKSUM_COMPLETE, skb->csum covers the packet: an IPv4 address and
// its header checksum cancel, and the pseudo-header replace below takes the
// L4 change out; an IPv6 address has no header checksum, so its store
// recomputes.
static __always_inline int nat_rewrite(struct __sk_buff *skb, struct scratch *s, __u32 l3, int dst, __u16 nport)
{
	struct flow *f = &s->f;
	__u8 v6 = f->dst.family == AF_INET6_, p = f->rproto;
	int icmp = is_icmp(p), l4 = f->frag != 2 && (p == IPPROTO_TCP || p == IPPROTO_UDP || icmp);
	__u32 aoff = l3 + (v6 ? (dst ? 24 : 8) : (dst ? 16 : 12));
	__u32 csoff = f->l4 + (p == IPPROTO_TCP ? 16 : p == IPPROTO_UDP ? 6 : 2);
	__u32 poff = f->l4 + (icmp ? 4 : (dst ? 2 : 0));
	__u64 mz = p == IPPROTO_UDP ? BPF_F_MARK_MANGLED_0 : 0;
	__u16 oport = pick(dst, f->dport, f->sport);
	__s64 diff;
	int i;

#pragma unroll
	for (i = 0; i < 4; i++)
		s->o[i] = pick(dst, f->dst.addr[i], f->src.addr[i]);
	diff = bpf_csum_diff(s->o, 16, s->n, 16, 0);
	if (diff < 0)
		return -1;
	if (!v6 && bpf_l3_csum_replace(skb, l3 + 10, 0, diff, 0) < 0)
		return -1;
	if (l4 && p != IPPROTO_ICMP &&
	    bpf_l4_csum_replace(skb, csoff, 0, diff, BPF_F_PSEUDO_HDR | mz) < 0)
		return -1;
	if (bpf_skb_store_bytes(skb, aoff, s->n, v6 ? 16 : 4, v6 ? BPF_F_RECOMPUTE_CSUM : 0) < 0)
		return -1;
	if (l4 && f->ported && oport != nport) {
		s->np = bpf_htons(nport);
		if (bpf_l4_csum_replace(skb, csoff, bpf_htons(oport), s->np, 2 | mz) < 0)
			return -1;
		if (bpf_skb_store_bytes(skb, poff, &s->np, 2, 0) < 0)
			return -1;
	}
	return 0;
}

// ---- MSS ---------------------------------------------------------------------

// One TCP option per call: clamps an MSS option to s->mss in the packet. The
// kernel's stacks put it first, and a SYN has few.
static long mss_step(__u32 i, void *ctx)
{
	struct walk_ctx *wc = ctx;
	struct scratch *s = scr();
	__u8 *b;

	if (!s || s->o_off >= s->o_end)
		return 1;
	b = s->eh;
	if (bpf_skb_load_bytes(wc->skb, s->o_off, b, 1) < 0 || b[0] == 0)
		return 1;
	if (b[0] == 1) { // NOP
		s->o_off += 1;
		return 0;
	}
	if (s->o_off + 2 > s->o_end || bpf_skb_load_bytes(wc->skb, s->o_off + 1, b + 1, 1) < 0 || b[1] < 2)
		return 1;
	if (b[0] != 2) {
		s->o_off += b[1];
		return 0;
	}
	if (b[1] != 4 || bpf_skb_load_bytes(wc->skb, s->o_off + 2, &s->omss, 2) < 0)
		return 1;
	if (bpf_ntohs(s->omss) > s->mss) {
		s->np = bpf_htons(s->mss);
		if (bpf_l4_csum_replace(wc->skb, s->f.l4 + 16, s->omss, s->np, 2) == 0)
			bpf_skb_store_bytes(wc->skb, s->o_off + 2, &s->np, 2, 0);
	}
	return 1;
}

static __noinline int clamp_mss(struct __sk_buff *skb)
{
	struct walk_ctx wc = { .skb = skb };
	struct scratch *s = scr();

	if (!s)
		return 0;
	if (bpf_skb_load_bytes(skb, s->f.l4 + 12, s->eh, 1) < 0)
		return 0;
	s->o_off = s->f.l4 + 20;
	s->o_end = s->f.l4 + (__u32)(s->eh[0] >> 4) * 4;
	bpf_loop(MSS_OPTIONS, mss_step, &wc, 0);
	return 0;
}

// ---- classification ------------------------------------------------------------

// Whether list id (a policy's includes or excludes) holds the flow's
// destination, and its port where the longest prefix limits them.
static __always_inline int dest_matches(struct scratch *s, __u32 id)
{
	struct flow *f = &s->f;
	__u32 *set;

	if (f->dst.family == AF_INET_) {
		struct dest4_key *k = &s->dk4;

		k->prefixlen = 64;
		k->id = id;
		k->addr = f->dst.addr[0];
		set = bpf_map_lookup_elem(&pod_dest4, k);
	} else {
		struct dest6_key *k = &s->dk6;

		k->prefixlen = 160;
		k->id = id;
		__builtin_memcpy(k->addr, f->dst.addr, 16);
		set = bpf_map_lookup_elem(&pod_dest6, k);
	}
	if (!set)
		return 0;
	if (*set == 0)
		return 1;
	if (!f->ported)
		return 0;
	s->dpk.prefixlen = 64;
	s->dpk.set = *set;
	s->dpk.proto = f->rproto;
	s->dpk.pad = 0;
	s->dpk.port = bpf_htons(f->dport);
	return bpf_map_lookup_elem(&pod_dest_ports, &s->dpk) != 0;
}

// One policy of the pod-set per call, in rank order (D24).
static long policy_step(__u32 i, void *ctx)
{
	struct scratch *s = scr();
	__u32 *id;

	if (!s)
		return 1;
	s->psk.index = i;
	id = bpf_map_lookup_elem(&pod_set_policies, &s->psk);
	if (!id)
		return 1;
	s->pol = *id;
	if (dest_matches(s, s->pol * 2 + 1))
		return 0; // excluded
	if (dest_matches(s, s->pol * 2)) {
		s->found = 1;
		return 1;
	}
	return 0;
}

// One candidate of the slice per call: the first free port. The insert of
// the translated tuple is the claim on it.
static long nat_probe(__u32 i, void *ctx)
{
	struct scratch *s = scr();
	__u16 cand;

	if (!s)
		return 1;
	cand = s->pstart + ((s->ph + i * ((s->ph >> 16) | 1)) & s->pmask);
	s->nxk.lport = cand;
	if (bpf_map_update_elem(&nat, &s->nxk, &s->nv, BPF_NOEXIST) == 0) {
		s->xport = cand;
		s->found = 1;
		return 1;
	}
	return 0;
}

// ---- Packet Too Big to the pod ---------------------------------------------------

// The pod's oversize packet comes back to it as an error from the address it
// was sent to, before translation (design 6.13). bpf_redirect_peer isn't
// available at egress, so it goes out the pod's host-side device, addressed
// to the pod's own MAC: the frame here carries the node's.
static __always_inline int pod_ptb(struct __sk_buff *skb, struct egress_config *c, struct scratch *s, __u32 mtu)
{
	if (!ptb_allowed()) {
		count(R_PTB_LIMITED, s->f.dst.family);
		return TCX_DROP;
	}
	s->ptb_mtu = mtu;
	if (bpf_skb_load_bytes(skb, ETH_HLEN, s->q, PTB6_QUOTE) < 0) {
		count(R_PTB_FAILED, s->f.dst.family);
		return TCX_DROP;
	}
	s->eth[12] = s->f.dst.family == AF_INET6_ ? 0x86 : 0x08;
	s->eth[13] = s->f.dst.family == AF_INET6_ ? 0xdd : 0x00;
	if (s->f.dst.family == AF_INET6_)
		return ptb6(skb, c, s);
	return ptb4(skb, c, s);
}

// ---- pl_pod_out ------------------------------------------------------------------

// Where a translated packet goes: direct, in the slot's table, or into the
// tunnel, its slot in the mark for pl_steer.
static __always_inline int pod_send(struct __sk_buff *skb, struct egress_config *c, struct scratch *s,
				    __u8 target, __u32 table)
{
	if (target == TARGET_TUNNEL) {
		set_mark(skb, c, s->slot);
		return bpf_redirect(c->tun_ifindex, 0);
	}
	__builtin_memcpy(s->f.src.addr, s->n, 16);
	return forward(skb, s, table);
}

static __always_inline int pod_out(struct __sk_buff *skb, __u32 l3)
{
	struct egress_config *c;
	struct scratch *s;
	struct pod_dev *pd;
	struct pod_slot *sl;
	struct pod_policy *pp;
	struct nat_val *v;
	struct flow *f;
	__u32 *ifx;
	__u64 now;
	__u16 rport, lport, xport;
	__u8 fam, target, *reserved;
	__u32 table, mtu;
	int whole, fail_open = 0;

	s = scr();
	c = config();
	if (!s || !c)
		return TCX_NEXT;
	// The pod's entry is copied, here and in the source lookup: a pointer from
	// either of two lookups is what the verifier refuses (design 6.6).
	s->ifindex = skb->ingress_ifindex;
	s->have_pd = 0;
	pd = bpf_map_lookup_elem(&pod_devs, &s->ifindex);
	if (pd) {
		if (!pd->pod_set)
			return TCX_NEXT;
		__builtin_memcpy(&s->pd, pd, sizeof(s->pd));
		s->have_pd = 1;
	} else if (!c->pod_by_src) {
		return TCX_NEXT; // not from a pod: the fast path
	}
	if (parse_ip(skb, s, l3, skb->protocol) < 0)
		return TCX_NEXT;
	f = &s->f;
	if (!s->have_pd) {
		// netkit: no ingress device. pl_pod_tag's source check at the peer
		// hook makes the source trustworthy.
		ifx = bpf_map_lookup_elem(&pod_src, &f->src);
		if (!ifx)
			return TCX_NEXT;
		s->ifindex = *ifx;
		pd = bpf_map_lookup_elem(&pod_devs, &s->ifindex);
		if (!pd || !pd->pod_set)
			return TCX_NEXT;
		__builtin_memcpy(&s->pd, pd, sizeof(s->pd));
	}
	fam = f->src.family;
	// Only the pod's own address: a translated packet round again, a tunnel's
	// outer header and what a CNI already translated all differ.
	if (fam == AF_INET_) {
		if (!s->pd.has_v4 || f->src.addr[0] != s->pd.v4)
			return TCX_NEXT;
	} else if (!s->pd.has_v6 || f->src.addr[0] != s->pd.v6[0] || f->src.addr[1] != s->pd.v6[1] ||
		   f->src.addr[2] != s->pd.v6[2] || f->src.addr[3] != s->pd.v6[3]) {
		return TCX_NEXT;
	}
	s->pod_set = s->pd.pod_set;
	__builtin_memcpy(s->eth, s->pd.mac, 6);
	__builtin_memset(s->eth + 6, 0, 6);
	s->ptb_if = s->ifindex;
	if (parse_l4(skb, s) < 0)
		return TCX_NEXT;
	now = bpf_ktime_get_ns();

	// A later fragment follows its first.
	if (f->frag == 2) {
		struct frag_key *k = &s->fk;
		struct nat_frag *nf;

		__builtin_memset(k, 0, sizeof(*k));
		__builtin_memcpy(k->src, f->src.addr, 16);
		__builtin_memcpy(k->dst, f->dst.addr, 16);
		k->id = f->frag_id;
		k->family = fam;
		k->proto = f->fproto;
		nf = bpf_map_lookup_elem(&nat_frags, k);
		if (!nf) {
			if (to_cluster(s))
				return TCX_NEXT;
			count(R_FRAG_UNTRACKED, fam);
			return TCX_DROP;
		}
		if (nf->slot == 0)
			return TCX_NEXT;
		s->slot = nf->slot;
		__builtin_memcpy(s->n, nf->addr, 16);
		s->kslot = s->slot;
		sl = bpf_map_lookup_elem(&pod_slots, &s->kslot);
		if (!sl) {
			count(R_NAT_NO_SLOT, fam);
			return TCX_DROP;
		}
		target = sl->target;
		table = sl->table;
		if (target == TARGET_PROHIBIT) {
			count(R_NO_HOLDER, fam);
			return TCX_DROP;
		}
		if (nat_rewrite(skb, s, l3, 0, 0) < 0) {
			count(R_MALFORMED, fam);
			return TCX_DROP;
		}
		count(R_NAT_OUT, fam);
		return pod_send(skb, c, s, target, table);
	}

	if (f->rproto == IPPROTO_TCP && bpf_skb_load_bytes(skb, f->l4 + 13, &s->tcpfl, 1) < 0)
		return TCX_NEXT;
	if (f->rproto != IPPROTO_TCP)
		s->tcpfl = 0;
	if (f->rproto == IPPROTO_TCP && (s->tcpfl & (TCP_SYN | TCP_ACK)) == TCP_SYN)
		count(R_POD_OUT_SYN, fam);

	// ICMP errors and echo replies answer something; they aren't flows.
	if (f->err)
		return TCX_NEXT;
	if (is_icmp(f->rproto) && s->l4b[0] == (fam == AF_INET_ ? 0 : 129))
		return TCX_NEXT;

	rport = 0;
	lport = 0;
	if (f->ported) {
		lport = f->sport;
		rport = is_icmp(f->rproto) ? 0 : f->dport;
	}
	nat_key_of(&s->nk, f->dst.addr, f->src.addr, rport, lport, f->rproto, fam, NAT_OUT);
	v = bpf_map_lookup_elem(&nat, &s->nk);
	if (v) {
		s->slot = v->slot;
		xport = v->port;
		__builtin_memcpy(s->n, v->addr, 16);
		s->kslot = s->slot;
		sl = bpf_map_lookup_elem(&pod_slots, &s->kslot);
		if (!sl) {
			count(R_NAT_NO_SLOT, fam);
			return TCX_DROP;
		}
		__builtin_memcpy(&s->sl, sl, sizeof(s->sl));
		nat_key_of(&s->nxk, f->dst.addr, s->n, rport, xport, f->rproto, fam, NAT_IN);
		v = bpf_map_lookup_elem(&nat, &s->nxk);
		if (v) {
			v->ts = now;
			v->state = nat_state(v->state, f->rproto, s->tcpfl, 0);
		}
	} else {
		// Not a new flow: TCP past its SYN, or a reply into a flow the pod
		// was sent.
		if (f->rproto == IPPROTO_TCP && (s->tcpfl & (TCP_SYN | TCP_ACK)) != TCP_SYN) {
			count(R_NAT_MIDSTREAM, fam);
			return TCX_NEXT;
		}
		if (f->rproto != IPPROTO_TCP) {
			__u64 *seen = bpf_map_lookup_elem(&pod_inbound, &s->nk);

			if (seen && now - *seen < INBOUND_TTL_NS) {
				count(R_INBOUND_REPLY, fam);
				return TCX_NEXT;
			}
		}
		if (to_cluster(s))
			return TCX_NEXT;
		// The policies in rank order; the first whose destinations match
		// decides (D24).
		s->psk.pod_set = s->pod_set;
		s->psk.family = fam;
		s->psk.pad = 0;
		s->found = 0;
		bpf_loop(POD_POLICIES, policy_step, 0, 0);
		if (!s->found)
			return TCX_NEXT; // not egress
		pp = bpf_map_lookup_elem(&pod_policies, &s->pol);
		if (!pp)
			return TCX_NEXT;
		fail_open = pp->fail_open;
		whole = pp->whole;
		s->slot = pp->slot;
		if (s->slot == 0) {
			if (fail_open)
				return TCX_NEXT;
			count(R_FAIL_CLOSED, fam);
			return TCX_DROP;
		}
		s->kslot = s->slot;
		// A slice carries TCP, UDP and ICMP echo. SCTP's CRC32c can't follow
		// a port rewrite here (user decision 2026-11-03), so it joins the
		// protocols without ports (D4).
		if (!whole && !(f->rproto == IPPROTO_TCP || f->rproto == IPPROTO_UDP ||
				(is_icmp(f->rproto) && s->l4b[0] == (fam == AF_INET_ ? 8 : 128)))) {
			__u64 *pl = bpf_map_lookup_elem(&pod_portless, &s->kslot);

			if (pl)
				*pl += 1;
			if (fail_open)
				return TCX_NEXT;
			return TCX_DROP;
		}
		sl = bpf_map_lookup_elem(&pod_slots, &s->kslot);
		if (!sl) {
			count(R_NAT_NO_SLOT, fam);
			return TCX_DROP;
		}
		__builtin_memcpy(&s->sl, sl, sizeof(s->sl));
		__builtin_memcpy(s->n, s->sl.addr, 16);

		// The inbound entry, whose insert claims the port; the outbound one
		// names it.
		__builtin_memset(&s->nv, 0, sizeof(s->nv));
		__builtin_memcpy(s->nv.addr, f->src.addr, 16);
		s->nv.port = lport;
		s->nv.ts = now;
		s->nv.state = NS_NEW;
		s->nv.slot = s->slot;
		nat_key_of(&s->nxk, f->dst.addr, s->n, rport, lport, f->rproto, fam, NAT_IN);
		s->pk.family = fam;
		s->pk.proto = f->rproto;
		s->pk.port = lport;
		__builtin_memcpy(s->pk.addr, f->src.addr, 16);
		reserved = bpf_map_lookup_elem(&pod_reserved, &s->pk);
		s->found = 0;
		if (whole || (reserved && *reserved == s->slot)) {
			// The port the pod chose: a whole address, or a reserved port.
			if (bpf_map_update_elem(&nat, &s->nxk, &s->nv, BPF_ANY) == 0) {
				s->xport = lport;
				s->found = 1;
			}
		} else {
			s->ph = bpf_get_hash_recalc(skb);
			s->pstart = s->sl.first;
			s->pmask = (__u32)s->sl.size - 1;
			if (s->sl.size && !(s->sl.size & s->pmask))
				bpf_loop(NAT_PROBES, nat_probe, 0, 0);
		}
		if (!s->found) {
			count(R_NAT_INSERT_FAILED, fam);
			return TCX_DROP;
		}
		xport = s->xport;
		s->nxk.lport = xport;
		__builtin_memcpy(s->nv.addr, s->n, 16);
		s->nv.port = xport;
		if (bpf_map_update_elem(&nat, &s->nk, &s->nv, BPF_ANY) < 0) {
			bpf_map_delete_elem(&nat, &s->nxk);
			count(R_NAT_INSERT_FAILED, fam);
			return TCX_DROP;
		}
		count(R_NAT_NEW, fam);
	}
	target = s->sl.target;
	table = s->sl.table;
	s->mss = s->sl.mss;
	mtu = s->sl.mtu;
	if (target == TARGET_PROHIBIT) {
		count(R_NO_HOLDER, fam);
		return TCX_DROP;
	}
	// Too big for the path, and not to be fragmented (IPv6 never is; IPv4
	// with DF). GSO packets are segmented later; the clamp sized them.
	if (l3 == ETH_HLEN && mtu >= PTB_MIN_MTU && !skb->gso_size && skb->len > l3 + mtu &&
	    (fam == AF_INET6_ || (s->hdr[6] & 0x40)))
		return pod_ptb(skb, c, s, mtu);
	// A first fragment leaves its address for the others.
	if (f->frag == 1) {
		struct frag_key *k = &s->fk;

		__builtin_memset(k, 0, sizeof(*k));
		__builtin_memcpy(k->src, f->src.addr, 16);
		__builtin_memcpy(k->dst, f->dst.addr, 16);
		k->id = f->frag_id;
		k->family = fam;
		k->proto = f->fproto;
		__builtin_memset(&s->nf, 0, sizeof(s->nf));
		__builtin_memcpy(s->nf.addr, s->n, 16);
		s->nf.slot = s->slot;
		bpf_map_update_elem(&nat_frags, k, &s->nf, BPF_ANY);
	}
	if (nat_rewrite(skb, s, l3, 0, xport) < 0) {
		count(R_MALFORMED, fam);
		return TCX_DROP;
	}
	if (s->mss && f->rproto == IPPROTO_TCP && (s->tcpfl & TCP_SYN))
		clamp_mss(skb);
	count(R_NAT_OUT, fam);
	return pod_send(skb, c, s, target, table);
}

SEC("tcx/egress")
int pl_pod_out_eth(struct __sk_buff *skb)
{
	return pod_out(skb, ETH_HLEN);
}

SEC("tcx/egress")
int pl_pod_out_l3(struct __sk_buff *skb)
{
	return pod_out(skb, 0);
}

// ---- pod_reply --------------------------------------------------------------------

// An ICMP error to the egress address, quoting a translated packet: the outer
// destination, the quoted source address and port (or echo id), the quoted
// IPv4 header checksum and quoted L4 checksum, and the ICMP checksum by the
// sum of them all (plus the pseudo header for ICMPv6). parse_l4 left the quote
// in s->qh and s->qp. 0, or < 0 to drop.
static __always_inline int nat_icmp_error(struct __sk_buff *skb, struct scratch *s, __u32 l3)
{
	struct flow *f = &s->f;
	__u32 l4 = f->l4, il3 = l4 + 8, il4, csoff, poff, total, ldiff;
	__u8 v6 = f->dst.family == AF_INET6_, *ih = s->qh, ip;
	__u16 xp, rp, op, oipcs = 0;
	struct nat_val *v;
	__s64 adiff, pdiff, cdiff;
	int have_ics = 0;

	if (v6) {
		ip = ih[6];
		il4 = il3 + 40;
	} else {
		if ((ih[0] >> 4) != 4 || (ih[0] & 0xf) < 5)
			return -1;
		ip = ih[9];
		il4 = il3 + (ih[0] & 0xf) * 4;
	}
	if (ip == IPPROTO_TCP || ip == IPPROTO_UDP) {
		xp = (__u16)s->qp[0] << 8 | s->qp[1];
		rp = (__u16)s->qp[2] << 8 | s->qp[3];
		poff = 0;
		csoff = ip == IPPROTO_TCP ? 16 : 6;
	} else if (is_icmp(ip)) {
		xp = (__u16)s->qp[4] << 8 | s->qp[5];
		rp = 0;
		poff = 4;
		csoff = 2;
	} else {
		return -1;
	}
	__builtin_memset(s->o, 0, 16);
	__builtin_memset(s->n, 0, 16);
	if (v6) {
		__builtin_memcpy(s->o, ih + 8, 16);
		__builtin_memcpy(s->n, ih + 24, 16);
	} else {
		__builtin_memcpy(&s->o[0], ih + 12, 4);
		__builtin_memcpy(&s->n[0], ih + 16, 4);
		oipcs = (__u16)ih[10] | (__u16)ih[11] << 8; // as in memory
	}
	nat_key_of(&s->nxk, s->n, s->o, rp, xp, ip, f->dst.family, NAT_IN);
	// The error must be addressed to the quoted (translated) source.
	if (s->o[0] != f->dst.addr[0] || s->o[1] != f->dst.addr[1] || s->o[2] != f->dst.addr[2] ||
	    s->o[3] != f->dst.addr[3])
		return -1;
	v = bpf_map_lookup_elem(&nat, &s->nxk);
	if (!v)
		return -1;
	__builtin_memcpy(s->n, v->addr, 16);
	op = v->port;

	adiff = bpf_csum_diff(s->o, 16, s->n, 16, 0);
	s->ow = bpf_htons(xp);
	s->nw = bpf_htons(op);
	pdiff = bpf_csum_diff(&s->ow, 4, &s->nw, 4, 0);
	if (adiff < 0 || pdiff < 0)
		return -1;
	// The quoted L4 checksum: pseudo header (not for ICMPv4) and port.
	ldiff = csum_add(pdiff, ip == IPPROTO_ICMP ? 0 : adiff);
	if (bpf_skb_load_bytes(skb, il4 + csoff, &s->ics, 2) == 0 && !(ip == IPPROTO_UDP && s->ics == 0)) {
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
		return -1;
	total = csum_add(csum_add(adiff, pdiff), cdiff);

	if (bpf_skb_store_bytes(skb, l3 + (v6 ? 24 : 16), s->n, v6 ? 16 : 4, v6 ? BPF_F_RECOMPUTE_CSUM : 0) < 0)
		return -1;
	if (!v6 && bpf_l3_csum_replace(skb, l3 + 10, 0, adiff, 0) < 0)
		return -1;
	if (bpf_skb_store_bytes(skb, il3 + (v6 ? 8 : 12), s->n, v6 ? 16 : 4, 0) < 0)
		return -1;
	s->np = bpf_htons(op);
	if (bpf_skb_store_bytes(skb, il4 + poff, &s->np, 2, 0) < 0)
		return -1;
	if (!v6 && bpf_skb_store_bytes(skb, il3 + 10, &s->nipcs, 2, 0) < 0)
		return -1;
	if (have_ics && bpf_skb_store_bytes(skb, il4 + csoff, &s->nics, 2, 0) < 0)
		return -1;
	if (bpf_l4_csum_replace(skb, l4 + 2, 0, total, 0) < 0)
		return -1;
	if (v6 && bpf_l4_csum_replace(skb, l4 + 2, 0, adiff, BPF_F_PSEUDO_HDR) < 0)
		return -1;
	return 0;
}

// A packet to this node's slice: translated back, and into its pod past the
// CNI. On a tunnel without an Ethernet header too: the peer's receive path
// starts at the network header (measured).
static __noinline int pod_reply(struct __sk_buff *skb, __u32 l3)
{
	struct scratch *s = scr();
	struct flow *f;
	struct nat_val *v;
	struct nat_frag *nf;
	__u32 *ifx;
	__u16 rport, lport, port = 0;
	__u8 fam;

	if (!s)
		return TCX_DROP;
	f = &s->f;
	fam = f->dst.family;
	s->mss = 0;
	if (f->err) {
		if (nat_icmp_error(skb, s, l3) < 0) {
			count(R_NAT_MISS, fam);
			return TCX_DROP;
		}
		count(R_NAT_ICMP_ERROR, fam);
	} else if (f->frag == 2) {
		__builtin_memset(&s->fk, 0, sizeof(s->fk));
		__builtin_memcpy(s->fk.src, f->src.addr, 16);
		__builtin_memcpy(s->fk.dst, f->dst.addr, 16);
		s->fk.id = f->frag_id;
		s->fk.family = fam;
		s->fk.proto = f->fproto;
		nf = bpf_map_lookup_elem(&nat_frags, &s->fk);
		if (!nf) {
			count(R_FRAG_UNTRACKED, fam);
			return TCX_DROP;
		}
		__builtin_memcpy(s->n, nf->addr, 16);
		if (nat_rewrite(skb, s, l3, 1, 0) < 0) {
			count(R_MALFORMED, fam);
			return TCX_DROP;
		}
		count(R_NAT_REPLY, fam);
	} else {
		rport = 0;
		lport = 0;
		if (f->ported) {
			lport = f->dport;
			rport = is_icmp(f->rproto) ? 0 : f->sport;
		}
		nat_key_of(&s->nxk, f->src.addr, f->dst.addr, rport, lport, f->rproto, fam, NAT_IN);
		v = bpf_map_lookup_elem(&nat, &s->nxk);
		if (!v) {
			count(R_NAT_MISS, fam);
			return TCX_DROP;
		}
		if (f->rproto == IPPROTO_TCP && bpf_skb_load_bytes(skb, f->l4 + 13, &s->tcpfl, 1) < 0)
			return TCX_DROP;
		if (f->rproto != IPPROTO_TCP)
			s->tcpfl = 0;
		__builtin_memcpy(s->n, v->addr, 16);
		port = v->port;
		s->slot = v->slot;
		v->ts = bpf_ktime_get_ns();
		v->state = nat_state(v->state, f->rproto, s->tcpfl, 1);
		if (f->frag == 1) {
			__builtin_memset(&s->fk, 0, sizeof(s->fk));
			__builtin_memcpy(s->fk.src, f->src.addr, 16);
			__builtin_memcpy(s->fk.dst, f->dst.addr, 16);
			s->fk.id = f->frag_id;
			s->fk.family = fam;
			s->fk.proto = f->fproto;
			__builtin_memset(&s->nf, 0, sizeof(s->nf));
			__builtin_memcpy(s->nf.addr, s->n, 16);
			s->nf.slot = s->slot;
			bpf_map_update_elem(&nat_frags, &s->fk, &s->nf, BPF_ANY);
		}
		if (nat_rewrite(skb, s, l3, 1, port) < 0) {
			count(R_MALFORMED, fam);
			return TCX_DROP;
		}
		if (f->rproto == IPPROTO_TCP && (s->tcpfl & TCP_SYN)) {
			struct pod_slot *sl;

			s->kslot = s->slot;
			sl = bpf_map_lookup_elem(&pod_slots, &s->kslot);

			if (sl && sl->mss) {
				s->mss = sl->mss;
				clamp_mss(skb);
			}
		}
		count(R_NAT_REPLY, fam);
	}
	// s->n is the pod's address.
	__builtin_memset(&s->ak, 0, sizeof(s->ak));
	__builtin_memcpy(s->ak.addr, s->n, 16);
	s->ak.family = fam;
	ifx = bpf_map_lookup_elem(&pod_src, &s->ak);
	if (!ifx) {
		count(R_POD_GONE, fam);
		return TCX_DROP;
	}
	s->ifindex = *ifx;
#pragma unroll
	for (int i = 0; i < MAX_VLAN_TAGS; i++) {
		if (!skb->vlan_present && skb->protocol != ETH_P_8021Q_BE && skb->protocol != ETH_P_8021AD_BE)
			break;
		if (bpf_skb_vlan_pop(skb) < 0) {
			count(R_MALFORMED, fam);
			return TCX_DROP;
		}
	}
	return bpf_redirect_peer(s->ifindex, 0);
}

// ---- pl_pod_tag's part ---------------------------------------------------------------

// On an eBPF-stage node pod traffic doesn't reach the forward hook, so the
// drop of a pod's UDP to a node's tunnel port (D20, resolution 15) is here,
// and a selected pod's SYN leaving the cluster is counted, against what
// pl_pod_out sees: rising alone, the CNI's datapath keeps it from the stage.
static __noinline int tag_stage(struct __sk_buff *skb, struct egress_config *c, struct scratch *s, __u8 pod_set)
{
	struct flow *f = &s->f;
	struct underlay_key *uk = &s->uk;

	if (parse_l4(skb, s) < 0 || f->frag == 2)
		return TCX_NEXT;
	if (f->rproto == IPPROTO_UDP) {
		if (bpf_htons(f->dport) != c->tun_port)
			return TCX_NEXT;
		__builtin_memset(uk, 0, sizeof(*uk));
		__builtin_memcpy(uk->addr, f->dst.addr, 16);
		uk->v6 = f->dst.family == AF_INET6_;
		if (bpf_map_lookup_elem(&node_by_underlay, uk)) {
			count(R_TUNNEL_PORT, f->dst.family);
			return TCX_DROP;
		}
		return TCX_NEXT;
	}
	if (f->rproto != IPPROTO_TCP || !pod_set)
		return TCX_NEXT;
	if (bpf_skb_load_bytes(skb, f->l4 + 13, &s->tcpfl, 1) < 0)
		return TCX_NEXT;
	if ((s->tcpfl & (TCP_SYN | TCP_ACK)) == TCP_SYN && !to_cluster(s))
		count(R_POD_SYN, f->dst.family);
	return TCX_NEXT;
}

// ---- pl_pod_inbound -------------------------------------------------------------------

// Its own buffers: this runs on the socket path, not the tc one.
struct inbound_scratch {
	__u8 hdr[40];
	__u8 l4[4];
	__u8 pad[4];
	struct addr_key dst;
	struct nat_key k;
	__u64 now;
	__u32 ifindex;
	__u32 src[4];
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct inbound_scratch);
} inbound_scr SEC(".maps");

// What is delivered to a selected pod's socket, however it got there
// (bpf_redirect_peer included), other than TCP's and ICMP's: the pod's replies
// to it are no new flows. The packet starts at the network header here, and
// extension headers aren't walked: the kernel has already taken a fragment
// header out, and a flow with others is left unrecorded. Always allows.
SEC("cgroup_skb/ingress")
int pl_pod_inbound(struct __sk_buff *skb)
{
	__u32 z = 0, *ifx, l4;
	struct inbound_scratch *s = bpf_map_lookup_elem(&inbound_scr, &z);
	struct pod_dev *pd;
	__u64 *seen;
	__u16 sport = 0, dport = 0;
	__u8 proto;

	if (!s)
		return 1;
	__builtin_memset(&s->dst, 0, sizeof(s->dst));
	__builtin_memset(s->src, 0, sizeof(s->src));
	if (skb->protocol == ETH_P_IP_BE) {
		if (bpf_skb_load_bytes(skb, 0, s->hdr, 20) < 0)
			return 1;
		proto = s->hdr[9];
		if (proto == IPPROTO_TCP || proto == IPPROTO_ICMP)
			return 1;
		s->dst.family = AF_INET_;
		__builtin_memcpy(&s->dst.addr[0], s->hdr + 16, 4);
		__builtin_memcpy(&s->src[0], s->hdr + 12, 4);
		l4 = (__u32)(s->hdr[0] & 0xf) * 4;
	} else if (skb->protocol == ETH_P_IPV6_BE) {
		if (bpf_skb_load_bytes(skb, 0, s->hdr, 40) < 0)
			return 1;
		proto = s->hdr[6];
		if (proto == IPPROTO_TCP || proto == IPPROTO_ICMPV6_ || ipv6_ext(proto))
			return 1;
		s->dst.family = AF_INET6_;
		__builtin_memcpy(s->dst.addr, s->hdr + 24, 16);
		__builtin_memcpy(s->src, s->hdr + 8, 16);
		l4 = 40;
	} else {
		return 1;
	}
	ifx = bpf_map_lookup_elem(&pod_src, &s->dst);
	if (!ifx)
		return 1;
	s->ifindex = *ifx;
	pd = bpf_map_lookup_elem(&pod_devs, &s->ifindex);
	if (!pd || !pd->pod_set)
		return 1;
	// Ports where the protocol has them in the first four bytes.
	if (proto == IPPROTO_UDP || proto == IPPROTO_SCTP) {
		if (bpf_skb_load_bytes(skb, l4, s->l4, 4) < 0)
			return 1;
		sport = (__u16)s->l4[0] << 8 | s->l4[1];
		dport = (__u16)s->l4[2] << 8 | s->l4[3];
	}
	nat_key_of(&s->k, s->src, s->dst.addr, sport, dport, proto, s->dst.family, NAT_OUT);
	s->now = bpf_ktime_get_ns();
	seen = bpf_map_lookup_elem(&pod_inbound, &s->k);
	if (seen) {
		*seen = s->now;
		return 1;
	}
	bpf_map_update_elem(&pod_inbound, &s->k, &s->now, BPF_ANY);
	count(R_INBOUND_RECORDED, s->dst.family);
	return 1;
}
