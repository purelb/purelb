# Phase 0 egress spike results

Evidence for the decisions in the egress plan (Phase 0b/0c). Every result
below was produced by code in this directory and can be re-run:

- VM spikes (1, 2, 4, 6, 7): `hack/egress-spikes/vm/vm-test.sh`, which boots
  each kernel of the matrix in QEMU with the egress module set
  (`BPF_VM_MODULES=egress hack/bpf-vm/kernels.sh`).
- Cluster spikes (3, 5): `hack/egress-spikes/cluster/spike3.sh`, `spike5.sh`.
- kwok (8): `hack/egress-spikes/kwok/` (its own README and RESULTS).

The prototypes are spike code, not production code.

## Kernel matrix

All VM spikes pass on every kernel of the matrix (run 2026-10-19):

| Kernel | Spike 1 | Spike 2 | Spike 4 | Spike 6 | Spike 7 |
|---|---|---|---|---|---|
| Debian 6.6.15 (floor) | 8/8 pass, 4/4 expected-fail | pass | pass (detach after close took 65 ms) | as below | pass |
| Ubuntu 6.8.0-146 | 8/8, 4/4 | pass | pass | as below | pass |
| Debian 6.12.111 | 8/8, 4/4 | pass | pass | as below | pass |
| Ubuntu 6.17.0-1022-azure (verifier-bug kernel) | 8/8, 4/4 | pass | pass | as below | pass |
| Debian 6.18.12 (verifier-bug kernel) | 8/8, 4/4 | pass | pass | as below | pass |

The address guard's own VM tests now also run, and pass, on 6.6.15
(`hack/bpf-vm/test.sh`); 6.6 had never been tested before.

## Spike 1: the tunnel (D3)

Topology: pod, pod node (nodeA), announcer (nodeB), server, as network
namespaces in each VM. nodeA SNATs into its slice and steers through
`purelb-eg0`; nodeB forwards transit out of its pool interface and returns
the reply to nodeA by slice; nodeA de-NATs it for the pod. The server checks
it sees the egress address with a port inside nodeA's slice.

| Variant | Result |
|---|---|
| L3 Geneve (`innerprotoinherit`) + Ethernet header pushed before transit | **works**: inner v4/v6 × underlay v4/v6 |
| L2 Geneve with `purelb-eg0` NOARP + MAC and `pkt_type` rewrite on tunnel ingress | **works**: inner v4/v6 × underlay v4/v6 |
| L3 Geneve, transit without pushing an Ethernet header | **fails, as predicted**: `bpf_redirect_neigh` drops an skb with no MAC header |

`pkt_type` after tunnel ingress was `PACKET_HOST` for every reply. Throughput
inside a VM over veth is not representative (40–50 Gbit/s on the host
kernel); bare-metal numbers belong to the performance spike.

**Design consequences**

- **L3 transit must push an Ethernet header** (`bpf_skb_change_head`) before
  `bpf_redirect_neigh`.
- **L2 mode needs `purelb-eg0` NOARP**; without it the node ARPs for the remote
  destination across the tunnel and ARP frames carry no tunnel key.
- **The steering program must clear the mark field**, or the Geneve outer
  route lookup (which uses `skb->mark`) loops; confirmed by the passing runs
  with the clear in place.
- **Strict `rp_filter` drops tunnel replies** unless the reply mark (0x80) is
  set by the tunnel-ingress program, `src_valid_mark=1` is on `purelb-eg0`,
  a mark rule sends the reverse lookup to a table whose default is
  `purelb-eg0`, and that table throws the pod CIDR back to main. With that
  in place every reply passed under `rp_filter=1`. IPv6 has no `rp_filter`.
- **Slice index is `port >> shift`** (slices are aligned on absolute port
  numbers). An implementation that subtracts a base port mis-indexes most of
  a slice.

### Spike 1 gaps (2026-10-19): fragments and the tunnel MTU

`TestSpike1GapFragments` (`gaps_test.go`), every matrix kernel, same results
on all five.

**Fragment steering.** With two announcers, across all four inner/underlay
combinations, no fragmented datagram was ever split across announcers
(`bpf_get_hash_recalc` keeps a datagram's fragments together). For IPv4
inner traffic it hashes addresses only, so all fragmented traffic between
one pod node and one server takes one announcer, and a flow's fragments and
its unfragmented packets can use different announcers; IPv6 inner traffic
stays per flow.

**Fragment delivery, with the design's `frags` handling (§6.5) added to the
prototype.** A first fragment carries the ports, so its slice owner is
looked up as usual and recorded in an LRU `frags` map (IPv4: addresses, IP
ID, protocol; IPv6: addresses and the 32-bit Fragment Header ID); later
fragments take the recorded owner; a later fragment with no entry is dropped
and counted. The entry stores the slice owner, never the sender, so a forged
first fragment cannot authorise someone else's fragments. Every kernel, all
49 tests pass:

| Inner / underlay | Pod → server: delivered (3000-byte datagrams) | Server → pod reply, fragments fit the tunnel | Reply, fragments too large for the tunnel |
|---|---|---|---|
| IPv4 / IPv4 | 12 / 12 | 4 / 4 | 4 / 4 (outer IPv4 fragmented) |
| IPv4 / IPv6 | 12 / 12 | 4 / 4 | **0 / 4** |
| IPv6 / IPv4 | 12 / 12 | 4 / 4 | 4 / 4 (outer IPv4 fragmented) |
| IPv6 / IPv6 | 12 / 12 | 4 / 4 | **0 / 4** |

No datagram was split across announcers, and no entry was missing except in
the IPv6-underlay oversize case: the announcer's kernel drops the oversize
outer packet (`Ip6FragFails`, Packet Too Big sent only to itself), so only
the last fragments reach the pod node, where they are dropped as missing.
The fragment code cost `pl_announce` 675-782 verified instructions (was
418) and `pl_tun_in` 1,247 (2,007 on 6.6; was 733 / 1,233); no verifier
rejection. Not covered here: reordered fragments (no buffering), and IPv6
extension headers before the Fragment Header (both covered in round 2, below). Behaviour change: later ICMP
fragments whose first fragment was not an echo are now dropped by
`pl_announce` (they used to pass to the node's stack).

**The tunnel MTU (a 1500-byte DF inner packet over a 1500-byte underlay):**

| Underlay | Pod node's target route | Result |
|---|---|---|
| IPv4 | no `mtu` | the outer packet is fragmented and reassembled at the announcer; the pod gets no signal |
| **IPv6** | no `mtu` | **the outer packet is not fragmented: dropped** (`Ip6FragFails`), the pod gets no signal |
| either | `mtu`, IPv4 inner | frag-needed reaches the pod, which learns the path MTU |
| either | `mtu` (not locked), **IPv6 inner** | **no Packet Too Big: dropped silently** |
| either | `mtu lock`, IPv6 inner | Packet Too Big reaches the pod, which learns the path MTU |

**Design consequences**

- **IPv6 target routes need `mtu lock`**, or oversize IPv6 packets from pods
  are dropped with no signal.
- **An IPv6 underlay drops, rather than fragments, oversize tunnel packets,
  in both directions** (forward and reply, measured). §6.7's "an oversize
  reply is fragmented by the underlay" holds only for an IPv4 underlay. With
  IPv6, an oversize reply that MSS clamping does not cover is lost, and the
  server never learns a smaller path MTU because no Packet Too Big reaches
  it. Options: an underlay MTU at least 56 bytes above the pool interface's
  MTU, or `pl_announce` fragmenting or answering with Packet Too Big itself.
- **The design's `frags` handling works as written**, for transit and both
  reply hops, both families.
- In the IPv4-underlay case the server counted the delivered datagrams as
  checksum errors although the captured bytes were valid: most likely stale
  checksum state across veth links in the VM, not seen on hardware
  (unproven).

## Spike 2: verifier, under the agent's exact capabilities (CAP_BPF + CAP_NET_ADMIN)

All four prototypes load on every matrix kernel, including the 6.17 and
6.18 builds with the speculation-barrier verifier bug.

| Program | Verified instructions (6.6 / 6.12) |
|---|---|
| `pl_steer` (tcx egress, `purelb-eg0`) | 210 / 176 |
| `pl_announce` (tcx ingress, uplink) | 418 / 418 |
| `pl_tun_in` (tcx ingress, `purelb-eg0`) | 1,233 / 733 |
| `pl_pod_tag` (tcx ingress, host-side pod device) | 175 / 175 |

Without CAP_PERFMON the verifier refused six constructs on the way. Each
became a coding rule the production programs must follow:

1. **No variable-offset direct packet access.** The speculation sanitizer
   allows variable offsets only on stack and map-value pointers, so
   `data + l4` is refused even when bounded. Read variable-offset fields with
   `bpf_skb_load_bytes` into a per-CPU scratch map, and write them with
   `bpf_skb_store_bytes` and the checksum-replace helpers (the address guard
   already works this way).
2. **No parse structs on the stack.** A field-by-field built struct overlaps
   spilled pointers ("attempt to corrupt spilled pointer on stack"). Build
   them in the scratch map.
3. **No path-dependent pointer arithmetic.** When two branches fill
   different fields or call a helper with different arguments, clang can
   merge them into one pointer computation whose offset depends on the path
   ("tried to add from different maps, paths or scalars"). Write shapes that
   store scalars at fixed offsets: for the tunnel key, both families store
   into the shared word `remote_ipv4` / `remote_ipv6[0]` unconditionally, and
   one helper call takes the family flag as a scalar.
4. **One load length per call site.** Two `bpf_skb_load_bytes` calls of
   different lengths into one buffer were merged the same way; load 8 bytes
   once for TCP, UDP and ICMP.
5. **Choose values, not addresses.** Selecting the source or destination
   port by index became a path-dependent offset; read both and select the
   value.
6. **`__sk_buff->mark` must be written as 32 bits.** clang narrows a masked
   update to a 1-byte store, which the verifier refuses; an asm barrier keeps
   it a 4-byte store.

### Spike 2 gaps closed (2026-10-19): the eBPF-NAT stage and one `pl_announce`

`hack/egress-spikes/vm/natspike` (run with `SPIKES_PKG=natspike vm-test.sh`)
adds a prototype of Phase 7's eBPF pod-node stage with PureLB-owned NAT
(D32): `pl_pod_out` SNATs a selected pod's flows to the egress address and
a free port inside the node's slice (bounded probing), keeps both directions
in a HASH map, and rewrites headers and checksums; `pl_pod_reply` reverses
it, including an ICMP error's quoted header. `pl_announce_all` is the
design's single `pl_announce`, with the uplink and tunnel-ingress paths in
one program.

| Kernel | Load under CAP_BPF + CAP_NET_ADMIN | `pl_pod_out` | `pl_pod_reply` | `pl_announce_all` |
|---|---|---|---|---|
| 6.6.15 | pass | 2,287 | 9,680 | 1,746 |
| 6.8.0-146 | pass | 2,045 | 5,356 | 1,130 |
| 6.12.111 | pass | 2,886 | 8,422 | 1,130 |
| 6.17.0-1022-azure | pass | 2,838 | 4,966 | 1,130 |
| 6.18.12 | pass | 2,839 | 4,966 | 1,130 |

(verified instructions). Functional, IPv4 and IPv6, every kernel, all 14
cases pass: TCP (server sees the egress address and a port inside the slice;
data round-trips), UDP, ICMP echo (identifier mapped and restored), an ICMP
port-unreachable de-NATed through the quoted header (the pod's socket gets
"connection refused"), fragments dropped and counted, conntrack untouched
(`nf_conntrack_count` 0; the NAT lives only in PureLB's map), and expiry
(TCP after FIN, UDP and ICMP after their timeouts).

Two more verifier rules, both without CAP_PERFMON:

7. **No ternary between two map values.** clang turns
   `v6 ? pn->eg6[0] : pn->eg4` into a choice of addresses; load both and
   combine them with a mask behind an asm barrier.
8. **Probing loops go in `bpf_loop`.** A plain bounded 16-candidate probe
   loop was refused on 6.12 ("sequence of 8193 jumps is too complex") and
   hit verifier EFAULT on 6.6 and 6.8; 6.17 and 6.18 accepted it. Moved into
   a `bpf_loop` callback that writes its result to the scratch map, it loads
   everywhere.

Not covered here (the attach point was covered in round 2, below):

- **Attach point.** The prototype runs at the pod veth's tcx ingress, before
  routing, where IPv4 then sees the node's own egress address as the source
  and drops it as martian unless `accept_local=1` on that veth (verified by
  removing it; IPv6 needs nothing). The design (§6.4) attaches at the host
  NIC's tcx egress, which should not need it; that point was not tested.
- **`pl_announce_all` carries no traffic yet**: loaded only; its two halves
  carried traffic separately in spike 1.
- Not implemented in the prototype: fragment tracking (drop and count was
  chosen), MSS clamping, frag-needed / Packet Too Big synthesis, tunnel
  steering from `pl_pod_out`, IPv6 extension headers other than the fragment
  header, and CHECKSUM_COMPLETE packets from real NICs (veth exercises only
  CHECKSUM_PARTIAL and none).

### Spike 1 and 2 gaps, round 2 (2026-10-20): attach point, one `pl_announce`, fragments, extension headers, PTB

Kernel-VM matrix (6.6.15, 6.8.0-146, 6.12.111, 6.17.0-1022-azure,
6.18.12-bpo). Load tests under `bpf,net_admin` only; functional tests with
the extra capabilities they need to build netns topologies. Root package:
76 pass / 0 fail / 1 skip per kernel (the skip is the cluster-only
`TestSpike3MarkOnNode`); natspike 6 / 0 / 0 per kernel. Logs:
`scratchpad/logs/fullroot/`, `scratchpad/logs/fullnat/`.

**eBPF NAT at the design's attach point (natspike).** `pl_pod_out` moved to
the host NIC's tcx egress (after routing), finding the pod by
`skb->ingress_ifindex`; `pl_pod_reply` stays at the uplink's tcx ingress.
`accept_local` is 0 everywhere and the test asserts it. All 14 cases pass in
both families on every kernel, plus a new 4 MiB bulk TCP echo each way
(203-293 translated skbs per run, so GSO/TSO packets translate correctly):
`tcp PASS: server saw 172.30.10.110:21326 (slice 20480-24575) ... 4 MiB bulk
echoed through 233 translated skbs` (6.12, IPv4). Control, the same program
at pod-veth ingress without `accept_local`: IPv4 fails on every kernel
(`i/o timeout; rt_cache in_martian_src +2`), IPv6 connects. So the design's
attach point needs no `accept_local`. Not run: `bpf_redirect_peer` into the
pod, and `pl_pod_flow`.

**One `pl_announce` carrying traffic (root package).** `pl_announce_all`
(uplink and tunnel-ingress bodies in one program, dispatched on
`skb->ifindex == tun_ifindex`, including `frags`, the extension-header walk
and PTB) on nodeB's pool interface, nodeB's `purelb-eg0` and nodeA's
`purelb-eg0`: 8 variants (inner v4/v6 x underlay v4/v6 x L3-with-Ethernet-
push/L2) x 6 cases, 48 PASS per kernel. Owner-local passes
(`owner_local +1`); an owner-0 slice drops (`no_owner +1`); in-slice transit
reaches the server; a transit source port in another node's slice drops
(`transit_forged +1`); a non-node outer source drops
(`transit_no_sender +1`); a reply for the node's own slice is admitted
(`reply_local +1`) and one for another node's slice drops
(`reply_not_owner +1`). natspike's own `pl_announce_all` copy is unchanged
and still load-only.

**Reordered fragments.** Crafted [F2, F1, F3], server to pod through
`pl_announce` and `pl_tun_in`, and as transit Geneve; all kernels, all four
inner/underlay combinations, identical: in-order control delivered;
[F2, F1, F3] `delivered=false (announcer first/followed/missing +1/+1/+1)`
-- the early F2 is dropped and counted; re-sending F2 (same ID) recovers the
datagram (`followed +1`) while the receiver still holds F1 and F3; a new
datagram in order is delivered. Drop-and-count behaves as specified: a
datagram whose non-first fragment arrives before its first is lost unless
that fragment is sent again within the receiver's reassembly timeout.
(netem is not in the VM module set; reordering used crafted fragments.)

**IPv6 extension headers.** A bounded walk (at most 8 headers: Hop-by-Hop,
Routing, Destination Options, Fragment, AH) in a `bpf_loop` callback. All
kernels, both underlays, reply and transit:
`[HBH, DestOpts, UDP] delivered=true (announcer to_tunnel +1, nodeA
reply_local +1)`; `[HBH, Frag, DestOpts, UDP] x3 delivered=true (announcer
to_tunnel +3, frags first/followed/missing +1/+2/+0, nodeA reply_local +3)`.
Found on the way: the `frags` key must use the Fragment Header's own next
header, or a first fragment (walked to UDP) and a later one (next header
Destination Options) get different keys. Before the walk (from the code,
not measured), such packets had `ok=0`: `pl_announce` passed them to the
stack and transit dropped them.

**PTB from `pl_announce` (resolution 1b).** IPv6 underlay, tunnel MTU 1400,
the server replying with 3000-byte datagrams fragmented at 1500:

| Server family | Sent by `pl_announce` | Server side | Delivery |
|---|---|---|---|
| IPv4 | `frag-needed sent=2` (type 3 code 4, MTU 1400, 576 bytes, valid IP and ICMP checksums) | `IcmpInDestUnreachs=+2 IcmpInErrors=+1`; route MTU to the egress address 1400 | first batch 3/4, second batch 4/4 with no further errors |
| IPv6 | `packet-too-big sent=2` (type 2, MTU 1400, 1280 bytes quoting 1232, valid checksum) | `Icmp6InPktTooBigs=+2`; route MTU 1400 | first batch 3/4, second batch 4/4 with no further errors |

IPv4 underlay: no errors sent, 4/4 delivered through outer IPv4
fragmentation (unchanged). Built in place (`bpf_skb_change_tail`, then
`bpf_skb_change_head`), MACs swapped, checksums by chunked `bpf_csum_diff`
(at most 512 bytes per call), returned with `bpf_redirect(skb->ifindex, 0)`.

- Only the first fragment's error updates the server's path MTU: an error
  quoting a non-first fragment cannot be matched to a socket (IPv4 counts it
  in `IcmpInErrors`). Linux accepted frag-needed for the non-DF IPv4
  fragments, though RFC 1191 does not require it to.
- Prototype gaps: no rate limit, GSO skbs skipped, `tun_mtu` must be at
  least 1280; a CHECKSUM_PARTIAL skb (possible on veth ingress) was not
  tested.

**Verified instructions** under `bpf,net_admin` (6.6 / 6.8 / 6.12 / 6.17 /
6.18):

| Program | Before (2026-10-19) | Now |
|---|---|---|
| `pl_announce` (with the walk and PTB) | 675-782 | 1,506 / 1,561 / 1,561 / 1,550 / 1,564 |
| `pl_tun_in` | 2,007 on 6.6, 1,247 elsewhere | 2,524 / 1,519 / 1,519 / 1,519 / 1,519 |
| `pl_announce_all` (root) | -- | 3,899 / 3,019 / 3,019 / 3,003 / 3,017 |
| `pl_pod_out` (at NIC tcx egress) | unchanged | 2,287 / 2,045 / 2,886 / 2,838 / 2,839 |

`pl_steer`, `pl_pod_tag`, `pl_pod_reply` and natspike's `pl_announce_all`
are unchanged.

**A ninth verifier rule.** `attempt to corrupt spilled pointer on stack` on
every kernel: the skb pointer passed to `bpf_loop` in a stack context was
spilled to r10-8, and after inlining clang reused that slot for another
key. Fixed by putting the `bpf_loop` call in its own `__noinline`
subprogram. Rule 9: **a stack context passed to `bpf_loop` lives in its own
subprogram.**

## Spike 3: does the pod tag survive the CNI into netfilter? (D2)

`pl_pod_tag` at the head of one test pod's host-side tcx ingress chain marks
the pod's packets with slot 3. nft counts packets with that mark at
prerouting (-150), forward and postrouting, and separately counts every
packet from the pod's IPv4 address at prerouting.

| Cluster / CNI | Program already on the pod's chain | Pod packets reach netfilter | Tag at prerouting | Verdict |
|---|---|---|---|---|
| prox-purelb2, flannel (bridge `cni0`) | none | yes | 92–94% | survives |
| t2-calico, Calico iptables (routed `cali*`) | none | yes | 91–94% | survives |
| t2-misc, Canal (`cali*` + flannel VXLAN) | none | yes | 94% | survives |
| t2-misc, kube-router v2.11.1 (bridge `kube-bridge`, iptables firewall) | none | yes | 98% (89% at forward and postrouting) | survives, also through a kube-router restart during the window |
| t2-misc, Antrea v2.7.0 (OVS `br-int`, Geneve, kube-proxy iptables) | none | yes | 92% (86% at forward and postrouting) | survives through OVS, also through an antrea-agent restart during the window |
| t2-cilium, Cilium 1.20 legacy host routing (`lxc*`) | 1 (Cilium) | yes | **0%** | **overwritten by Cilium** |
| t2-calico-ebpf, Calico eBPF (`cali*`) | 1 (Calico) | yes | **0%** | **overwritten by Calico** |

The percentages are below 100% because the counting window starts after the
first tagged packets.

**IPv6 (2026-10-19, per-family counters).** Re-run with tagged packets and
pod-sourced packets counted per family at prerouting, 30 s of IPv4 and IPv6
pings to the gateway:

| CNI | IPv4: tagged / pod packets at prerouting | IPv6: tagged / pod packets at prerouting |
|---|---|---|
| flannel (prox-purelb2) | 24 / 24 | 31 / 26: survives |
| Calico iptables | 24 / 24 | 29 / 26: survives |
| Antrea (t2-misc) | 24 / 24 | 31 / 26: survives |
| Calico eBPF | 0 / 24 | 0 / 26: overwritten |
| Cilium, legacy host routing | 0 / 24 | 0 / 24: overwritten |

IPv6 behaves as IPv4 on every CNI. Where it survives, IPv6 counts above
100% because `pl_pod_tag` also tags the pod's link-local packets (neighbour
discovery), while the denominator counts only the pod's global address.

**Design consequences**

- **Bridge CNIs work with D2** (flannel's bridge does not touch the mark),
  which retires review blocker 3 for flannel, Calico iptables and Canal.
- **Cilium legacy routing overwrites the low mark bits** (the review's
  unverified concern about `set_identity_mark` is confirmed). That row needs
  the source-IP classification fallback in nft, relying on Cilium's own
  anti-spoofing.
- **Calico eBPF does not bypass netfilter for pod egress**, contrary to the
  original design's assumption. Its pod traffic reaches host prerouting with
  the pod source, but without the tag. It could use the netfilter stage with
  source-IP classification instead of the eBPF stage; whether Calico's BPF
  masquerade then leaves PureLB's SNAT alone is not yet tested here (tested
  later: "Calico eBPF: which stage works, and why").
- kube-router keeps the tag **with its service proxy off** (as built: kube-proxy
  runs services). Its firewall marks only the masked bits 0x10000 and
  0x20000 (v2.11.1 `pkg/controllers/netpol`). With `--run-service-proxy=true`
  its IPVS proxy marks each service with a hash in 0x0–0x3FFF
  (`pkg/controllers/proxy/utils.go`), which overlaps the mark byte. That
  configuration is IPVS-based, which the plan already reports as unsupported
  for egress; `validate` should name kube-router's proxy explicitly.
- **Antrea's OVS datapath carries the tag through** to host prerouting
  (Antrea's own Egress feature, which marks packets, was not enabled and is
  not measured). Antrea names the host-side veth after the pod
  (`purelb-s-9db899`), so device discovery must use the pod's `iflink`, not a
  name prefix.

### Calico eBPF: which stage works, and why (2026-10-20)

**Correction.** The first version of this section (28a) said PureLB's
netfilter SNAT never reaches the wire on Calico eBPF. That was wrong for
`natOutgoing: true`: both "PureLB rule loaded" runs printed byte-identical
router captures (ICMP ids 53049 / 14685, UDP ports 61371 / 1715) from two
different pods, and the capture file's mtime was 15:22:45, the earlier run
whose ruleset nft had rejected. They were reading a stale capture of the
no-rule baseline. The `natOutgoing: false` rows were real (fresh capture).

Re-run on t2-calico-ebpf-3 (Calico 3.33, BPF dataplane), test pod pinging,
sending UDP 9999 and a multi-packet TCP exchange (the router's SSH port) in
both families, egress addresses `172.30.250.25` / `2001:470:b8f3:250:11::5`
on `eth0`, router capture on FRR `ens19`, Calico's BPF debug log
(`bpfLogLevel: Debug`, removed afterwards) from `trace_pipe`:

| Stage | `natOutgoing` on (Calico's default) | `natOutgoing` off |
|---|---|---|
| **Netfilter** (28a's ruleset: `postrouting` priority 90, per-source-IP `snat` to the egress address, ports 20480-24575) | **works, both families, whole flows**: every packet of the TCP exchange in both directions carried the egress address and a port in the slice; pings 2/2; SSH banner received; nft counters matched | **fails**: nft counters 0, the pod's own address on the wire, no replies |
| **eBPF** (natspike `pl_pod_out` on `eth0` tcx egress, `pl_pod_reply` on its tcx ingress, `cluster/ebpf-stage.sh`), head or tail of Calico's chain | **fails**: `pl_pod_out` never matched (no NAT entries); the source was already the node's | **works, both families, head and tail**: egress address and slice ports / echo ids on the wire, pings 2/2, SSH banner received, 12 NAT entries |

**Why: Calico's program on the pod's interface picks one of two paths per
packet** (its debug log, `cali...-E`, the program on the host-side veth's
ingress):

- `natOutgoing` on, destination outside the pools: `Source is in
  NAT-outgoing pool but dest is not, need to SNAT.` -> `Disabling FIB lookup
  due to outgoing SNAT` -> `Traffic is towards host namespace, marking with
  0x1800000` -> `Final result=ALLOW rc -1`. The packet goes up the host
  stack, every packet of the flow (11 IPv4 and 13 IPv6 "Disabling FIB"
  lines for two pings, UDP and TCP). Netfilter then masquerades it:
  `table ip calico` / `table ip6 calico`, chain `nat-cali-nat-outgoing`,
  `meta mark & 0x01f00000 == 0x01800000 masquerade fully-random`, at
  `postrouting priority srcnat + 10`. PureLB's rule at priority 90 binds the
  connection first, so its SNAT wins. Calico's `eth0` egress program then
  sees the already-translated packet (`eth0------------E: IP
  s=172.30.250.119` in the no-PureLB run), which is why an eBPF stage on
  the NIC sees no pod source.
- `natOutgoing` off: `Traffic is towards the host namespace, doing Linux FIB
  lookup` -> `Got Linux FIB hit, redirecting to iface 2.` -> `Final
  result=ALLOW rc 7` (TC_ACT_REDIRECT), IPv4 and IPv6 alike. The packet is
  redirected straight to `eth0`'s transmit path; netfilter never sees it,
  but tcx egress on `eth0` does.

Calico's netfilter tables untrack nothing on this path
(`raw-cali-untracked-policy` matched 0 packets; `notrack` only for VXLAN).

**Test artifact (not a design issue).** The egress /128 was added to
`eth0` without `preferred_lft 0`, so IPv6 source selection (longest
matching prefix: `...250:11::5` against the router's `...250::1`) made it
the node's own IPv6 source, and Calico's IPv6 masquerade used it ("Calico's
choice, port 1715" above). PureLB's VIP path adds IPv6 addresses deprecated
for exactly this reason (`internal/local/announcer_local.go`), and egress
addresses are placed through it (D31).

### Attach order across a CNI restart

The same setup with `SPIKE3_RESTART_DS` set: our program at the head of the
pod device's tcx ingress chain, and the CNI's DaemonSet restarted (rolled
out) during a 120 s window. Chain listed head first; the revision counts
chain changes.

| CNI | Before restart | After restart | Revision | Reading |
|---|---|---|---|---|
| Cilium 1.20, legacy host routing | `[ours, 549]` | `[ours, 826]` | 3 → 5 | Cilium replaced its program with a separate attach and detach, behind ours |
| Calico 3.33 eBPF | `[ours, 577]` | `[ours, 861]` | 3 → 4 | Calico replaced its program in place (one change), behind ours |
| kube-router v2.11.1 | `[ours]` | `[ours]` | 2 → 2 | kube-router attaches nothing to the pod's device |
| Antrea v2.7.0 | `[ours]` | `[ours]` | 2 → 2 | Antrea attaches nothing to the pod's device (its datapath is the OVS kernel module) |

Neither CNI re-anchors at the head on restart, so a PureLB program at the
head stays first. This does not change the spike 3 result: on these two
CNIs the program behind ours still overwrites the tag.

## Spike 4: pinned links keep the [guard, pl_announce] order (D12)

Under only CAP_BPF + CAP_NET_ADMIN: a stand-in guard pinned at the head of a
dummy link's tcx ingress chain, `pl_announce` attached after it and pinned.
Every program, map and link fd closed (agent restart): both stay attached in
order. Reopened from the pins and upgraded with `bpf_link_update`: order
unchanged with the new program IDs. Unpin and close: both detach.

**Design consequences**

- **Pinning needs no CAP_SYS_ADMIN**: CAP_BPF and a writable bpffs directory
  suffice.
- **On 6.6, detach after the last close is asynchronous** (65 ms here); on
  6.8 and later it was immediate. Uninstall and teardown must wait for the
  detach to show in a chain query, not assume it.

## Spike 5: the CNI chain entry through a CNI restart and a reboot (D34, D35)

prox-purelb2 (flannel), run twice. The first run's pod check was broken by
the script (wrong namespace); the numbers below are from the second run. The
`purelb-cni-spike` DaemonSet (token-less, three hostPaths) adds a
`purelb-egress-spike` entry to the end of `10-flannel.conflist` on every
node, re-inserts it by inotify and atomic rename, and the plugin logs each
CNI call.

| Step | Result |
|---|---|
| Install | entry present on all 5 nodes once the DaemonSet reported ready |
| A pod created afterwards (`default/spike5-a`) | plugin saw its ADD, with the snapshot present |
| flannel DaemonSet restart (flannel's `install-cni` rewrites the conflist) | installer re-inserted the entry by inotify; no 0.2 s sample of the file ever lacked it |
| Node reboot | snapshot in `/var/lib/purelb` persisted; installer started 30 s after boot and re-inserted the entry within the same second, after flannel rewrote the file |
| First non-hostNetwork pod after boot (coredns, 31 s later) | plugin saw its ADD |
| Cleanup | entry, binary, state and image gone from all 5 nodes |

**Design consequences**

- The D34 installer works as designed on flannel: the entry survives the
  primary CNI rewriting its conflist, with a gap below the sampling interval.
- **Not exercised in this run: a pod sandbox created between flannel rewriting
  the file at boot and the installer re-inserting the entry** (exercised on
  2026-10-20, below). A pod in that window would bypass the plugin, so
  a `FailClosed` pod would start untagged. The production design needs an
  answer for it (for example the agent checking, on start, every pod sandbox
  on the node against the plugin's record, as Phase 6 already plans for pods
  that predate the plugin).
- The prototype's uninstall rewrites the conflist through `json.MarshalIndent`
  (sorted keys, new whitespace). The content is unchanged, but the production
  installer should remove only its own entry and leave the rest byte for byte.

### Spike 5: the boot window on flannel (2026-10-20)

`SPIKE5_BOOT_PODS=5`: five `restartPolicy: Always` pods pinned to purelb2-5
before the reboot, so the kubelet recreates their sandboxes as soon as it
starts, racing flannel and the installer.

**Script bug found and fixed first.** The after-boot report ran `ssh` inside
a `while read` loop; ssh read the loop's stdin, so only the first sandbox
was ever reported. Every earlier run (flannel and the Tier 2 rows below) was
affected, so their "only host-network pods started after boot" is not a
finding. `on()` now uses `ssh -n`, and the report gives per pod the
attempts, the successful sandbox and the plugin's ADDs, from containerd's
journal (the parser was checked against the first run's real journal).

Second run (boot at 15:41:07 UTC):

| Event | Time (UTC) |
|---|---|
| kubelet's first sandbox attempts for every pod | 15:41:33 |
| installer started and re-inserted the entry (flannel's `install-cni` had rewritten the file) | 15:41:34 |
| flannel-networked sandboxes failing (`open /run/flannel/subnet.env: no such file or directory`) | from 15:41:33, 4 failures per boot pod, 6 for coredns |
| successful sandboxes: coredns, spike5-boot-1..5 | 15:41:55 - 15:42:04 |

| Pod | hostNetwork | attempts | succeeded | plugin ADDs |
|---|---|---|---|---|
| spike5-boot-1..5 | false | 5 each | 15:41:56 - 15:42:04 | each: one before the reboot (15:39:24) and one at its successful sandbox |
| coredns | false | 7 | 15:41:55 | 15:41:55 |
| cni-spike, kube-proxy, flannel, lbnodeagent | true | 1 each | 15:41:33 | none (host network bypasses CNI) |

The first run (bug present, same node) showed the same shape from the
journal: the entry was back at 15:36:42 and the boot pods' sandboxes
succeeded at 15:37:06-08.

**Design consequences**

- On flannel the boot window is closed **by flannel, not by PureLB**: flannel
  fails every ADD until its daemon writes `subnet.env`, about 20 s after the
  installer's re-insert, so no pod could network without the entry. The
  installer's margin over the kubelet's first attempt was 1 s.
- A primary CNI that is ready before the installer pod starts would let a
  pod through without the plugin. The design still needs the agent-start
  check (Phase 6: every pod sandbox on the node against the plugin's record),
  and it is the only defence on such a CNI.
- Not re-run here on the Tier 2 rows (re-run later: "Boot window on the
  Tier 2 CNIs" and "The remaining Tier 2 rows").

### Spike 5 on the Tier 2 CNIs (2026-10-19)

Same script, each cluster's `-3` worker rebooted; cleanup restored every
node's conflist byte for byte.

| CNI | Entry through the CNI's DaemonSet restart | Reboot: snapshot kept, entry re-inserted | Verdict |
|---|---|---|---|
| Calico 3.33 (iptables) | no 0.2 s sample ever lacked it | yes | works, as on flannel |
| Calico 3.33 (eBPF) | no sample lacked it | yes | works |
| Antrea 2.7 | no sample lacked it; Antrea deletes and recreates its conflist, and the installer re-inserted within the same second | yes | works |
| **Cilium 1.20** | **a write war: 30 gaps of 0.2-0.7 s (40 of 600 samples without the entry)**; one node never showed the entry, even at install. Re-run: 32 gaps, 42 samples, and the entry was still missing when the 120 s window ended | entry re-inserted at boot (re-run, after a script fix) | **D34's re-insert approach fails** |

**Design consequences**

- **Cilium owns `/etc/cni/net.d`.** Its agent (`cni.exclusive`, the default)
  rewrites `05-cilium.conflist` whenever it changes, so an external edit is
  undone again and again and a pod created in a gap skips the egress plugin.
  On Cilium, PureLB has to chain through Cilium's own configuration, not by
  editing the file. D34 needs a Cilium-specific row. The working keys are
  below ("Cilium chaining through `cni.configMap`").
- **The boot window**: see "Boot window on the Tier 2 CNIs" below (re-run
  with the fixed script, 2026-10-20).

### Boot window on the Tier 2 CNIs (2026-10-20)

The fixed `spike5.sh` with `SPIKE5_BOOT_PODS=5` (Calico iptables, Calico
eBPF, Antrea; installer DaemonSet) and `spike5-cilium.sh` (Cilium; PureLB
chained through `cni.configMap`, no installer), each rebooting the `-3`
node. Every non-host-network pod sandbox created after boot, and whether the
plugin logged its ADD:

| CNI | Sandboxes after boot | Through the plugin | Attempts | How the window closed |
|---|---|---|---|---|
| Calico 3.33 iptables | 11 (5 test pods, coredns x2, calico-kube-controllers, csi-node-driver, calico-apiserver x2) | 11 / 11 | 1 each | The pods ran from the pre-reboot file, which still held the entry; Calico's `install-cni` rewrote it at 20:28:40, after them (20:28:27-39), and the installer re-inserted it in the same second |
| Calico 3.33 eBPF | 7 (5 test pods, calico-apiserver, csi-node-driver) | 7 / 7 | 1 each | Same as iptables: Calico's rewrite at about 20:28:07 came after the pods (20:27:56-20:28:04); re-inserted in the same second |
| Antrea 2.7 | 7 (5 test pods, coredns x2) | 7 / 7 | 3-13 | Antrea failed every ADD until its agent was ready (20:33:52); the installer had re-inserted at 20:33:44 |
| Cilium 1.20 (`cni.configMap`) | 5 | 5 / 5 | 1 each | The entry is in Cilium's own conflist, written at the Helm upgrade (20:24:21) and not rewritten at boot |

Each test pod also shows its pre-reboot ADD. CNI-restart sampling: 0
samples without the entry on all three installer rows. Cleanup restored
every conflist byte for byte (Cilium: Helm back to its starting revision,
values identical).

**Design consequences**

- No pod reached the network without the plugin on any of the five CNIs
  (with flannel, above).
- On Calico the CNI's own rewrite happens *after* the first pods, so there
  is a moment between that rewrite and the installer's re-insert; it was
  under one second here (below the 0.2 s sampling in the restart test). On
  Cilium there is none. The agent-start check (Phase 6: every pod sandbox on
  the node against the plugin's record) remains the defence for that moment
  and for any CNI that is ready before `purelb-cni` starts.

### The remaining Tier 2 rows (2026-10-20)

Run by one agent per cluster, each cluster restored afterwards (t2-calico
back to iptables, Installation and FelixConfiguration specs byte-identical;
t2-cilium back to profile (a), Helm values identical to the starting
revision; t2-misc rebuilt as Antrea and health-checked). New script
`cluster/nf-snat.sh`: the netfilter stage end to end (a pod, the egress
addresses on the uplink with IPv6 deprecated as PureLB's VIP path places
them, an nft postrouting table at priority 90 SNATing the pod's source into
20480-24575, traffic to FRR, FRR's capture filtered to the node's MAC).
PASS = only the egress addresses with slice ports at the router, pings 2/2,
both SSH banners back. Logs: `scratchpad/tier2-calico-nft/`,
`scratchpad/tier2-misc/`, `scratchpad/tier2-cilium-bc/`.

| Row | Spike 3 mark (v4 / v6) | tcx order on the pod device | Spike 5 restart / boot window | Netfilter SNAT end to end | eBPF stage (NIC head / tail) |
|---|---|---|---|---|---|
| flannel (prox-purelb2) | survives (earlier) | -- | 0 missing / 6 of 6 | **PASS** | -- |
| Calico iptables | survives (earlier) | not measured | 0 missing / 11 of 11 | **PASS** (both profiles of t2-calico) | -- |
| Calico nftables (Felix restarts in place, no rollout) | 100% / 100%, through a restart | [ours] (Calico attaches nothing) | 0 missing / 10 of 10 (as on iptables, Calico's rewrite at boot, 20:51:58, came after the first pods from 20:51:49; re-inserted the same second) | **PASS** | -- |
| Calico eBPF | overwritten | [ours, calico] | 0 missing / 7 of 7 | **PASS** with `natOutgoing` on | works with `natOutgoing` off only |
| Canal | 100% / 100% (first IPv6 run), through a restart | [ours] | 0 missing / 7 of 7 (Canal rewrites its conflist on restart and at boot; re-inserted at 20:53:06, pods from 20:53:30) | **PASS** | -- |
| kube-router (service proxy off, BGP on 1790) | 100% / 100% (first IPv6 run), through a restart | [ours] | 0 missing (never rewrites an existing conflist) / 5 of 5 | **PASS** | -- |
| Antrea | survives (earlier) | [ours] | 0 missing / 7 of 7 | **PASS** | -- |
| Cilium (a) legacy host routing | overwritten | [ours, cilium] | `cni.configMap`: 0 missing / 5 of 5 | **PASS** | -- |
| Cilium (b) BPF host routing, veth | no pod packet reaches netfilter | [ours, cilium] | `cni.configMap`: 6 of 6 | fails: netfilter never sees pod egress; Cilium's BPF masquerade puts the node address on the wire | **works at the head** (both families, whole flows, replies back; Cilium's masquerade leaves the translated packets alone); **never runs at the tail**: Cilium's eth0 programs end the tcx chain (`cil_to_netdev` run count rose, ours had none) |
| Cilium (c) BPF host routing, netkit | our tcx program on the netkit primary sees no packet at all (Cilium uses netkit's peer hook) | [ours] on tcx; Cilium on `netkit/peer` | `cni.configMap`: 5 of 5 | fails, as (b) | **head: runs but never matches the pod** (`nat_miss`, no entries; the packet still has the pod's source at `cil_to_netdev`, proven from Cilium's NAT map; inferred: `skb->ingress_ifindex` is not the netkit device, because Cilium's peer program redirects to eth0); tail: never runs |

- Switching Cilium between veth and netkit needs every pod on the node
  deleted first ("Cannot start cilium-agent with datapath-mode=netkit:
  detected 1 existing endpoint(s) using incompatible datapath-modes").
- Cilium treats every address added to eth0 as a node address
  (`reserved:host`) and recompiles and relinks `bpf_host` on eth0 when one
  changes (three times in about 60 s for one change). The relink is in
  place, so a PureLB program at the head stays at the head; the churn scales
  with egress-address changes.
- Not run in this pass (run later: "Cilium netkit: the designed path"): PureLB's designed netkit path, `pl_pod_tag` at
  `BPF_NETKIT_PEER` ahead of Cilium's peer program (the spike attached to
  the netkit primary's tcx hook). A direct read of `skb->ingress_ifindex`
  on a netkit node was blocked by the permission check and not run.
- The `hack/tier2` rebuild procedure failed after Canal: calico-node leaves
  a cgroup2 mount at `/run/calico/cgroup`; the README now unmounts it first.

### Cilium netkit: the designed path (2026-10-20)

t2-cilium switched to profile (c) (BPF host routing, `bpf.datapathMode=netkit`,
`netkit mode l3`), measured, and rolled back to Helm revision 14 (values
identical, legacy routing and veth again). Logs: `scratchpad/netkit-spike/`.

**What the peer hook and the NIC see** (`scratchpad/netkitdiag`, read-only
programs that return NEXT: one at the head of the pod's `BPF_NETKIT_PEER`,
one at the head of eth0's tcx egress; a pod pinging, UDP and TCP to FRR):

| Observation | Value |
|---|---|
| Peer-hook chain after attaching | `[nkdiag_peer, cil_from_container]`: ours ahead of Cilium |
| `skb->ifindex` at the peer hook | 81 = the host-side primary (`lxc...`), not the pod's eth0 (80) |
| `skb->ingress_ifindex` at the peer hook and at eth0 egress | **0** (no ingress device: the packet never passes a receive path) |
| First byte of data at the peer hook | `0x00` (unicast), `0x33` (IPv6 multicast): an Ethernet header is present even in L3 mode |
| `skb->mark` of the pod's packets at eth0 | `0xdd430f00` (Cilium's identity mark) |

So the earlier failure is explained: `pl_pod_out` found the pod by
`ingress_ifindex`, which is 0 on netkit (now measured, not inferred).

**`pl_pod_tag` at `BPF_NETKIT_PEER`** (`SPIKE3_ATTACH=netkit-peer`, keyed by
`skb->ifindex` as on veth, Ethernet-offset parsing unchanged): chain
`[ours, cil_from_container]` at start and end; it tagged the pod's packets
(20-40 per run). Spoofed sources (the pod adds `10.99.99.99` / `fd99::99`
with NET_ADMIN and pings from them; `SPIKE3_SPOOF`): IPv4 3 of 3 dropped;
IPv6 every packet sent was dropped (busybox ping stops at the first
`sendto` error, so it sends 1-2). Cilium's drop monitor (`cilium-dbg
monitor --type drop`) during a run with both families: **0 events with a
spoofed source** while ours dropped 5; the same pings with no PureLB program:
Cilium dropped 3 IPv4 and 1 IPv6 (`Invalid source ip`, `bpf_lxc.c:1723`).
PureLB's source check therefore runs before Cilium on netkit.

**`pl_pod_out` finding the pod by source address** (new `pod_src` map in
natspike, keyed by address and family, consulted when the `ingress_ifindex`
lookup misses; `EBPF_NAT_KEY=src`), at the head of eth0: **works, both
families**: the router saw only `172.30.250.35` and `2001:470:b8f3:250:12::5`
with ports 21015-23723, pings 2/2, both SSH banners, `nat_out=16 nat_new=6
nat_reply=12 nat_icmp_err=2`, 12 NAT entries. The lookup is safe because
`pl_pod_tag` has already dropped any other source at the peer hook. The
natspike VM matrix still passes on all five kernels (6 / 6; `pl_pod_out`
verified at 2,552-3,448 instructions with the extra lookup).

Cost note: in the prototype the source lookup runs after a full parse for
every NIC-egress packet whose `ingress_ifindex` misses (all non-pod traffic
on any node); restricting it to netkit nodes avoids that.

### CNI-agnostic alternatives: tested assessment (2026-10-21)

**Decided (user, 2026-10-21):** the Calico integration stays (A3 rejected for
its per-packet cost on every node); items 2, 5, 6 and 7 below were adopted.

Question: how to pick the pod-node stage without CNI-specific code (the
Calico `natOutgoing` handling of resolution 20 reads CNI configuration).
Driver `cluster/dual-stage.sh` (modes `dual|nf|ebpf|observe|pod`), both
families, router capture filtered by the node's MAC; logs
`scratchpad/dual/`. All clusters restored (Calico Installation, pools,
FelixConfiguration identical; Cilium Helm values identical; no test objects
left).

| Alternative | CNI knowledge | First packet | Two pools on one node | Result |
|---|---|---|---|---|
| A0: netfilter stage, require `natOutgoing` on (current) | Calico pool flag | correct with `natOutgoing` on | a pod in a non-NAT pool leaks its own address | as measured before |
| A1: read the pod's Calico IPPool | Calico IPPool CRDs (Calico only) | correct if the mapping is | handled | not built |
| A2: decide per pod from observed packets | none | **leaks** until decided (pod's own address with `natOutgoing` off, the node's with it on) or must drop | handled | IPv6 ND reaching prerouting gives a false "netfilter" signal unless only forwarded packets count |
| **A3: both stages armed, slice split in halves** (netfilter 20480-22527, eBPF 22528+2048 at the NIC head) | **none** | **correct from packet 1** | **passed** | passed on Calico eBPF (`natOutgoing` on, off, mixed pools on one node), Calico iptables, Cilium (a), Cilium (b), flannel; no packet reached the router with a non-egress source; every flow's port in exactly one half; replies to netfilter-translated flows pass `pl_pod_reply` (map miss) to conntrack |
| A5: NAT at the pod's host-side device, before the CNI | none | dropped | -- | **fails**: Calico workload RPF (`Workload RPF fail: not a local workload`, 12 IPv4 + 8 IPv6), Cilium anti-spoofing (`Invalid source ip`), IPv4 local-source drop on flannel |

Mixed-pool run (Calico eBPF, two pods on one node, pools with `natOutgoing`
true and false, sending at once): pod 1 left through the netfilter half
(v4 UDP 22426, v6 21107), pod 2 through the eBPF half (v4 22630, v6 23693);
both pods' pings and SSH banners returned; 12 eBPF NAT entries, all pod 2's.

A3 costs: each stage gets half the node's slice per address (2048 ports
each) unless the split adapts or a second slice is allocated; `pl_pod_reply`
on every uplink packet (+50-53 ns, spike 10) and `pl_pod_out` on every NIC
egress packet (40-42 ns miss); two NAT paths. It does not handle a CNI that
masquerades before netfilter priority 90 (none seen). Both stages must apply
the same destination selection.

**Item 2, pod address ranges.** `Node.spec.podCIDRs` covers every pod IP on
flannel and Cilium (IPAM `kubernetes`) but **none** on Calico (Calico IPAM
blocks, e.g. podCIDR 10.62.1.0/24, pods in 10.62.124.64/26). Calico IPPools
cover them; pod `status.podIPs` cover every running pod by construction.
Cilium cluster-pool IPAM: see "Pod ranges from kernel routes plus Node podCIDRs".

**Item 5, chain-entry installer without a Cilium branch.** With no back-off,
Cilium (a) rewrote the conflist 5-42 ms after each of our writes (median 10,
p99 29, n=1000) and a pod created meanwhile had no plugin ADD; Calico and
flannel rewrote once each, on restart, 77-85 s after our previous write.
Detecting 5 reverts within 10 s (revert window 1 s): Cilium detected 53 ms
after install, stop editing, `CNIChainMissing`; Calico and flannel never
triggered. With Cilium's `cni.configMap` the installer found the entry
already present and made no edits.

**Item 7, classification method chosen by the probe** (counting only
forwarded pod packets): flannel and Calico iptables -> mark; Calico eBPF
(`natOutgoing` on) and Cilium (a) -> source IP; Cilium (b) and Calico eBPF
(`natOutgoing` off) -> neither (eBPF stage). All match the Tier 2 results;
mark and source IP decided at the first forwarded packet, "neither" after 5
tagged packets with none forwarded (about 1 s).

**Item 6, attach order.** Every run attached at the head and stayed there;
no CNI program needed naming.

### Pod ranges from kernel routes plus Node podCIDRs (open point 1, 2026-10-21)

Question: agents watch only their own node's pods, so pod `status.podIPs`
cannot feed every node's cluster sets. Can each agent derive the pod ranges
from its own kernel routes plus every Node's `spec.podCIDRs`?

The route rule ("rule A"), from `ip -4/-6 -d route show table all`, keeps a
prefix when all of these hold:
- the table is not `local`;
- the type is not local, broadcast, multicast, anycast, throw or nat;
- it is not a default route, and not in fe80::/10 or ff00::/8;
- either the type is blackhole, unreachable or prohibit (any device; Calico's
  IPv6 blackhole prints `dev lo`), or the device is not an uplink, `lo` or
  `kube-lb0`.

An uplink is any device with a unicast default route in any non-local table,
in either family. On multi-homed purelb2-3, both eth0 (default in table 250)
and eth1 count.

Ground truth was the cluster CIDR plus the Calico IPPools. Two snapshots
were taken, before and after adding 2 test pods per cluster; scripts and
dumps are in `scratchpad/podroutes/`.

| Cluster (CNI) | Nodes | Missed by routes + podCIDRs | Routes only | podCIDRs only | False inclusions |
|---|---|---|---|---|---|
| prox-purelb2 (flannel VXLAN + DirectRouting) | 5 | **0** | 2-10 of 14 per node (same-subnet remotes go via the uplink) | 0 | none |
| t2-calico (iptables, VXLAN) | 3 | **0** | 0 | 17 of 18 (Calico IPAM blocks ignore podCIDRs) | none |
| t2-calico-ebpf (BPF, VXLAN) | 3 | **0** | 0 | 17 of 18 | 3 per node via `bpfin.cali`: kube-dns ClusterIP /32, 169.254.1.1/32, 2001:db8::1/128 |
| t2-cilium (geneve, IPAM kubernetes) | 3 | **0** | 0 | 0 | none |
| t2-misc (Antrea, geneve) | 3 | **0** | 0 | 0 | none |

Both families were measured on every node, in both snapshots. Neither half
works alone: routes alone fail on flannel DirectRouting, and podCIDRs alone
fail on Calico.

The Calico eBPF false inclusions are harmless. The cluster sets already hold
the Service and link-local networks (design §6.3), and 2001:db8::/32 is
documentation space.

**Timing.** A new Calico block was forced with the
`cni.projectcalico.org/ipAddrs` annotation. Every node was polled every
10 ms, with clocks within about 6 ms.

| Cluster | Local blackhole route, v4 / v6 (s) | Remote nodes' route, v4 / v6 (s) | Pod's own route (s) |
|---|---|---|---|
| t2-calico | 57.94 / 58.45 | 57.91-57.93 / 58.40-58.45 | 58.59 |
| t2-calico-ebpf | 65.19 / 65.67 | 65.20-65.22 / 65.63-65.64 | 65.73 |

Every node had the route before the pod's interface existed, so coverage lags
only as long as the agent takes to see the netlink route event.

**Direct routing with pod addresses outside the Node podCIDRs
(2026-10-22).** Each cluster was rebuilt in that mode, measured, then rebuilt
back. The first attempt switched a running Calico through its
`Installation`: the rollout deadlocked, because the first BIRD node fails its
startup probe until its peers run BIRD, and it left the IPv4 pool
half-changed. Building in the target mode avoids that (hack/tier2 README).
Both clusters were restored and verified: Calico `Installation` spec and
Cilium Helm values identical to the originals, routes back to their original
shape, and cross-node pod ping in both families. Evidence is in
`scratchpad/podroutes-direct/`.

The remote pod routes use the uplink, with the owning node's InternalIP as
next hop, in both families:
- Calico BGP: `10.61.153.128/26 via 172.30.250.112 dev eth0 proto bird`
- Cilium native: `fd63:0:0:80::/64 via 2001:470:b8f3:250:be24:11ff:feb1:ca85 dev eth0 proto kernel`

Cilium's routes carry `proto kernel`, so the proto does not mark them as the
CNI's.

The gateway clause adds to rule A: also keep an uplink route whose gateway is
some Node's `status.addresses` InternalIP or ExternalIP.

| Cluster (mode) | Pod IPs per node | Rule A + podCIDRs: missed | With the gateway clause: missed | Gateway clause false inclusions | podCIDRs only: missed |
|---|---|---|---|---|---|
| t2-calico (BGP mesh, encapsulation None) | 28 | 22 / 12 / 22 | **0 / 0 / 0** | none | 28 / 28 / 28 |
| t2-cilium (native, `autoDirectNodeRoutes`, cluster-pool) | 16 | 12 / 8 / 12 | **0 / 0 / 0** | none | 16 / 16 / 16 |

Both families were measured on every node. Both clusters publish IPv6
InternalIPs on their Nodes. The gateway clause found no IPv6 match on
prox-purelb2, whose Nodes publish only IPv4, but that cluster does not need
it: podCIDRs cover flannel.

In BGP mode, a new block's route reached the other nodes 29-40 ms (IPv4) and
22-27 ms (IPv6) after the owning node's blackhole, and before the pod's own
interface route.

Still not covered: remote pod routes whose next hop is not a Node address.
Examples are Calico peering with a top-of-rack router instead of the mesh,
and IPv6 next hops that are not published in `Node.status.addresses`.

### Mark bytes per CNI (resolution 22, 2026-10-20)

`cluster/markmasks.sh` reads every node's netfilter ruleset and reports the
bits each mark expression matches or modifies, and any rule that overwrites
or exactly matches the whole mark:

| CNI | Bytes used by netfilter rules | Free | Notes |
|---|---|---|---|
| flannel + kube-proxy (nftables) | `0x0000ff00` (`0x4000`) | `0x000000ff`, `0x00ff0000`, `0xff000000` | |
| Calico iptables / nftables | `0x0000ff00`, `0x00ff0000` (`0x001b4000`) | `0x000000ff` (`0xff000000` is reserved by Felix's `endpointMark=0xffe00000`) | identical on both dataplanes |
| Canal | as Calico | `0x000000ff` | Felix `IptablesMarkMask 0xffff0000` |
| kube-router | `0x0000ff00`, `0x00ff0000` | `0x000000ff`, `0xff000000` | |
| Antrea | `0x0000ff00`, `0xff000000` (`0x80004000`) | `0x000000ff`, `0x00ff0000` | |
| Calico eBPF | all four (`meta mark set meta mark & 0x03000000 \| 0x03000000` clears the rest) | none | tag overwritten; source-IP classification |
| Cilium (a), (b), (c) | all four (`& 0xfffffeff == 0xa00`; TPROXY `meta mark set 0x200` and exact matches on `0x<node-id>0200`); its BPF assigns the whole mark (`set_identity_mark`: cluster id in the low byte, magic `0x0F00`, key `0xF000`, identity in the upper 16 bits) | none | tag overwritten; source-IP classification |

**`0x000000ff` is the only byte free on every CNI that keeps the tag**, and
the only byte none of them reserves. On Calico eBPF and Cilium no byte is
free, but those rows do not classify by the tag; PureLB's slot mark is set
in its own nft stage after their pod-side programs. Cilium's TPROXY rule
(`meta mark set 0x200`) rewrites the whole mark of traffic redirected to
its L7 proxy, so such flows lose PureLB's slot.

### Cilium chaining through `cni.configMap` (2026-10-20)

On t2-cilium (Cilium 1.20.3, Helm revision 2: legacy host routing, Geneve,
dual-stack), with only the spike plugin binary installed on each node
(`/opt/cni/bin/purelb-egress-spike`, nothing editing `/etc/cni/net.d`):

1. A ConfigMap `kube-system/purelb-cilium-cni`, key `cni-config`, holding
   Cilium's own conflist with PureLB's plugin appended:
   `{"cniVersion":"1.0.0","name":"cilium","plugins":[{"type":"cilium-cni","enable-debug":false,"log-file":"/var/run/cilium/cilium-cni.log"},{"type":"purelb-egress-spike"}]}`
2. `helm upgrade cilium --reuse-values --set cni.configMap=purelb-cilium-cni`
   (`cni.configMapKey` left at its default `cni-config`; **`cni.exclusive`
   left at its default, true**).

| Check | Result |
|---|---|
| Conflist on all 3 nodes after the upgrade | `['cilium-cni', 'purelb-egress-spike']`; `05-cilium.conflist` is the only file |
| t2-cilium-3 sampled every 0.2 s through `rollout restart ds/cilium` + 30 s | 233 samples, **0 without the entry** |
| New pod on t2-cilium-3 | Ready, IPs 10.63.1.16 and fd63:0:0:1::97b5; the plugin logged `ADD ... args=default/spike28c` |
| Rollback (`helm rollback cilium 2`, ConfigMap, binary and state removed) | conflist back to `['cilium-cni']` on all nodes; Helm values identical to the saved revision-2 values |

**Design consequences**

- Cilium writes the PureLB entry itself, so there is nothing to fight over,
  and `cni.exclusive` does not need to be turned off (the 1.20.3 chart
  documents it as renaming *non-Cilium* configurations to `*.cilium_bak`;
  the entry lives inside Cilium's own file, so it is kept).
- The ConfigMap must carry Cilium's full plugin entry, so it is
  version-coupled to Cilium's own conflist: an operator who changes Cilium's
  CNI options must keep the ConfigMap in step. `purelb-cni` checks the
  rendered conflist for the entry and reports `CNIChainMissing` if it is
  absent.
- Not tested here: Cilium profiles (b) and (c) (eBPF host routing, netkit);
  their boot window through `cni.configMap` was run later ("The remaining
  Tier 2 rows": 6 of 6 and 5 of 5).
  The `cni.configMap` path is in the agent's CNI installer and independent of
  the datapath mode, but that is from the chart, not measured.

## Spike 6: IPv6 (D20, IPv6 subsection)

| Question | Result on every matrix kernel |
|---|---|
| Does per-device `net.ipv6.conf.<dev>.forwarding=1` forward IPv6 with `all.forwarding=0`? | **No.** `all.forwarding=1` is required |
| Output-hook drop of `ip6 saddr <egress address>` without exemptions | **breaks reachability**: the node's NA comes from the egress address and is dropped |
| Same drop with NS/NA (ICMPv6 135/136, hop limit 255) and `ct state established,related` exempted | **works** |

**Design consequences**

- Any node that forwards IPv6 pod traffic needs `net.ipv6.conf.all.forwarding=1`;
  per-device forwarding is not enough. `validate` must check it (and the
  RA-acceptance interaction it implies), or the agent must set it.
  (Settled by resolution 23: a `validate` check, no new hostPath.)
- The D20 output-hook drop must carry the ND and reply-direction exemptions.

### Spike 6 gaps (2026-10-19)

`gaps_test.go`, every matrix kernel, same results on all five.

- **The design's `ct direction reply` exemption** (in the output drop of
  egress-sourced packets, with the ND exemption) behaves like the tested
  `established,related` for IPv4 and IPv6: neighbour discovery stays
  REACHABLE; replies to connections that came in to the egress address
  leave; new connections originated from it are dropped. It is stricter in
  one case: a flow a local process opened from the egress address before
  the rule existed is dropped under `ct direction reply` and kept under
  `established,related`. Without the ND rule, IPv6 breaks (neighbour entry
  FAILED, inbound TCP "no route to host"); IPv4 is unaffected. A local
  process connecting to the egress address itself is dropped under every
  form (a Service with kube-proxy DNAT on a shared address was not tested).
- **The ND exemption on `announce_in` is unnecessary.** Neighbour
  discovery to the egress address passes the −150 chain with or without
  it, because conntrack marks NS and NA untracked, so `ct state
  new,invalid` never matches them (the chain was shown to be dropping
  other traffic). Harmless to keep.

## Spike 7: nftables on 6.6 (D18, D32)

A router marks only a flow's first packet (`ct state new`). Every later
packet must get its slot back from the conntrack mark through a vmap whose
per-slot chains write the mark with constant mask-and-xor, or the slot rule
does not route it (the main table blackholes the server). A 16 MiB stream
completed on every kernel, and the server saw the per-slot constant SNAT
address.

**Design consequences**

- The decided nft shape (constant-write ct-mark restore, per-slot constant
  `snat` rules, no register-to-register OR) loads and works on 6.6 through
  6.18, built with `google/nftables` v0.3.0.
- A pod node that is itself the announcer needs `src_valid_mark` on the
  pool interface (or loose `rp_filter`), because replies arrive there with
  the slot restored; the test only passed under strict `rp_filter` with it.

### Spike 7 gaps (2026-10-19): ICMP echo and NAT66

- **ICMP echo through the slice SNAT**, IPv4 and IPv6, every kernel: an echo
  identifier outside the slice is remapped into it (for example 4,660 became
  24,246 on 6.6 IPv4) and kept across sequence numbers; one already inside is
  kept; the server sees the egress address with valid checksums; replies
  reach the pod with its original identifier.
- **NAT66:** the per-slot constant SNAT with an IPv6 egress address loads on
  every kernel and translates TCP (a 4 MiB stream) and UDP (one mapping per
  flow, replies return, through the ct-mark restore), with the server seeing
  the egress address and a port inside the slice. Spike 1 already covered
  NAT66 for TCP through the tunnel.

## Spike 8: control-plane load under kwok (D5, D6, D17)

Full results, validity and tables: [`kwok/RESULTS.md`](kwok/RESULTS.md). Run on
a dedicated 16 vCPU / 31 GiB VM, a fresh kwok cluster (Kubernetes v1.35.9) per
run, 5,000 selected pods, with an egress-free baseline (`-design none`) for
comparison. The N=500 target was the assistant's choice, not the user's
(the user confirmed 500 nodes as the target on 2026-10-19; see the plan).

- **D6 as decided does not scale.** At N=500 it never settled, with 1,959 429s
  and lease renewals up to 67 s late. The baseline at N=500 settled with
  none of that, so the cause is the egress design. About 1,560 real changes
  produced 153,228 acks, because every node acks every change (one global
  epoch), and every node receives every whole EgressEndpoints object. A longer
  ack interval only slows this down.
- **Scoping acks and watches to the nodes involved removes it.** The
  `scoped` prototype (each address's slice table watched and acked only by
  its announcers; each node's own slices in its EgressNodeState spec, watched
  and acked only by that node; per-address and per-node versions) settled at
  N=500 with no 429s and no late renewals. It cut acks 27x and slice-table
  watch traffic 44x, with ack p99 under 1 s and apiserver CPU and memory close
  to the baseline. This would revise D5 and D6 and add a takeover re-read to
  the announcer; the decision is the user's (made 2026-10-19: D6 revised to
  the scoped form).
- **Claim latency** (a pod needing a new slice during a 5,000-pod rollout):
  89 / 128 s (p50 / p99) with one allocator writer, which leaves claims
  behind a full queue (measured); 43 / 63 s with 4 writers; 28 / 42 s with 4
  writers and a priority lane for the claim path; 12 / 25 s adding an
  allocator QPS of 200 and 4 reserve slices per address (claim bursts
  7 / 9 s), measured after correcting the prototype so a reserve claim also
  waits for the announcers' ack of its new owner. The allocator then writes about 60/s, bound by API round trips
  rather than its client limit; no run had 429s or late renewals.
- **With PureLB's shipped 10 s lease no run renewed late**, the decided
  design included; the earlier late counts came from the 5 s harness lease.
- **Agent QPS:** client-go's default 5/10 did not keep up at 500 addresses;
  50/100 did.
- **Takeover re-read (D6):** a by-name GET of an address's slice table
  through a random agent's own client, every 250 ms, scoped design with the
  decided settings, N=500, 500 addresses: idle 7 / 42 ms (p50 / p99),
  during the 5,000-pod rollout 25 / 410 ms, during the claim burst
  14 / 85 ms. At most about 0.4 s on top of the Lease expiry (10 s) that
  triggers a takeover. The same run measured claim latency 15 / 26 s against
  12 / 25 s in the previous run of the same configuration: run-to-run
  variation is about 3 s at the median.
- **Lease isolation (D-lease):** with the decided design under load,
  renewals ran late through the shared client and APF level. The harness
  lease is 5 s (the shipped default is 10 s), so the counts overstate what
  a 10 s lease would see.

## Spike 11: D7 aggregate mode, through the LB remote path (2026-10-19)

prox-purelb2, `hack/egress-spikes/cluster/spike11.sh`: a remote ServiceGroup
with `aggregation` at default (every node advertises `10.255.11.0/24` and
`fc00:255:11::/64`, inside the e2e routed bands) and a dual-stack Service.
The probe runs on a host **behind** FRR (the t2-kwok VM), so its traffic is
forwarded and ECMP-hashed like real replies, with redirects turned off on it
for the test; per-node counters confirm the spread (both families reached
all five nodes). The unclean failure drops every frame to or from FRR's MAC
on the node's uplink (nftables netdev ingress and egress), like a dead link.
The egress datapath does not exist yet, so this tests the routing aggregate
mode relies on.

| | BFD off (FRR hold time 9 s) | BFD on (300 ms x 3, both sides) |
|---|---|---|
| Aggregate from every node, both families | 5-way ECMP, 0 probes failed | same |
| Path withdrawn, graceful (reboot) | 4.3 s | **1.3 s** |
| Path withdrawn, unclean (dead link) | 7.8-9.4 s | **0.6 s** |
| Probes lost, unclean | IPv4: 4 of about 37 in the window; IPv6: 0 (FRR's kernel stops using an IPv6 next hop whose neighbor discovery fails, before BGP reacts; IPv4's equivalent, `fib_multipath_use_neigh`, is off) | not separable from the flapping below |

**Finding: k8gobgp's BFD does not recover.** With BFD on, every node whose
BGP session dropped (purelb2-5 after its reboot, purelb2-4 after its link
failure) went into a loop: BGP up, BFD "Expired" 3 s later, gobgpd itself
resets BGP ("BFD is down", Cease/administrative reset), BGP up again about
30 s later, and so on (16 and 20 expiries) until BFD was removed. BFD only
worked on sessions that were already established when it was switched on. A
BFD session that never came up should not take BGP down. Evidence:
k8gobgp logs and FRR BFD state, kept from the run. BFD was switched off
again on both sides; all five sessions returned and the configuration is as
before the test. Filed as purelb/gobgp-netlink#65.

Other observations:

- A node that comes back advertises its route before it can serve: a
  returning gateway must not advertise until its egress datapath is
  programmed (Phase 4 restart ordering).
- After a link failure, BGP took about 2 minutes to re-establish (BFD off),
  most likely gobgpd's connect-retry timer: capacity, not correctness.
- The graceful runs show 40-60 s of intermittent failures after the path
  was withdrawn: a backend pod was on the rebooted node and kube-proxy kept
  sending to it until Kubernetes marked it not ready. That is the LB
  backend's endpoint timeout and does not apply to egress.
- Earlier runs of this spike were invalid and are superseded: a probe on
  FRR itself (its own connections are not spread like forwarded traffic), a
  failure that only dropped traffic in the input and output hooks (which
  kube-proxy's DNATed traffic never crosses), and a probe host that took
  ICMPv6 redirects straight to one node.

**Design consequences**

- Aggregate mode works with PureLB's existing remote path. Failover is the
  BGP hold time without BFD, sub-second with BFD, but BFD must not be
  recommended for k8gobgp until its recovery bug is fixed.
- Phase 4: a returning gateway advertises only after its datapath is ready.

## Spike 9: cluster facts

prox-purelb2: Kubernetes API server v1.33.12 (kubelets v1.33.9), Debian 13
kernel 6.12.111, kube-proxy **nftables**. Tier 2 clusters: Kubernetes 1.35.9;
t2-calico runs kube-proxy nftables, t2-cilium and t2-misc iptables,
t2-calico-ebpf none.

## Spike 10: performance costs and proposed budgets (2026-10-20)

Kernel-VM matrix (6.6.15, 6.8.0-146, 6.12.111, 6.17.0-1022-azure,
6.18.12-bpo; 2 vCPU, 2 GiB), `hack/egress-spikes/vm/perf/vm-perf.sh`
(tests skip unless run with `-spike10`). Every number is min / median of 5
runs, from `SPIKE10` log lines (`scratchpad/logs/spike10/final/`). VM
numbers are relative; throughput and memory must be re-measured on real
hardware.

### Linux 6.6.15 leaks kernel memory on the tunnel path (resolved: kernel requirement)

**Decision (user, 2026-10-20):** egress requires a kernel with CVE-2025-40183
fixed (6.6.113, 6.12.54, 6.17.4, 6.18.0 or later, or a distribution
backport); the datapath keeps `bpf_redirect_neigh`, as Cilium does (its
`egress_gw_fib_lookup_and_redirect` and `fib_do_redirect` on `main` still
prefer it; no workaround in its code). The tunnel tests run on a fixed
6.6.y with a slab-growth soak.

On 6.6.15, every tunnel configuration (L3 and L2, IPv4 and IPv6 underlay,
both inner families, MTU 1500 and 9000) grows unreclaimable slab, all
`kmalloc-256`: `mtu1500/inner4/l3/underlay4` took Slab from 10.9 MB to
557.8 MB after 10 s of upload and 898.8 MB after 10 s of download
(`kmalloc-256=3541424x256(885356kB)`); plain routing and the direct path
stay flat. The 2 GiB VM was OOM-killed in the first run. 6.8, 6.12, 6.17
and 6.18 stay flat (6.8: Slab 18.6 -> 24.1 MB across all 24
configurations). Spike 1 sent only short traffic and never saw it.

**Not present on the latest 6.6 stable.** Ubuntu's mainline build of
6.6.159 (added to `hack/bpf-vm/kernels.sh` as `mainline-6.6.159`, outside
the default matrix): Slab 16.5 -> 22.0 MB across plain and the four
L3/L2 x IPv4/IPv6-underlay configurations, throughput in line with the
other kernels (tunnel 47-51 Gbit/s, plain 76-79). So it is a 6.6 stable
kernel bug fixed between 6.6.15 and 6.6.159.

**Root cause (bisected 2026-10-20).** One run of
`tunnel/mtu1500/inner4/l3/underlay4` per Ubuntu mainline 6.6.y build (slab
growth over the 10 s upload): 6.6.15 501,972 kB, 6.6.80 473,412, 6.6.110
503,768; 6.6.115 3,872, 6.6.125 3,840, 6.6.134 3,676, 6.6.159 4,168
(mainline has no builds between 6.6.110 and 6.6.115). The kernel.org
ChangeLogs for 6.6.111-6.6.115 contain, in **6.6.113**, commit
`b6bfe44b6dbb` = upstream `23f3770e1a53` "bpf: Fix metadata_dst leak
__bpf_redirect_neigh_v{4,6}" (Daniel Borkmann, 2025-10-03): a
collect-metadata tunnel attaches a `metadata_dst` to each decapsulated
packet, and `bpf_redirect_neigh()` set the new dst without dropping it,
leaking one `kmalloc-256` per packet (found by Cilium's egress gateway).
It `Fixes: b4ab31414970` (the helper's introduction), so every branch
without the fix leaks. First fixed releases (kernel.org ChangeLogs):
**6.6.113, 6.12.54, 6.17.4**, and 6.18.0. Upstream 6.8 reached end of life
before the fix; Ubuntu's 6.8.0-146 does not leak, so it carries a
backport. In the prototype the leaking call is `pl_tun_in`'s transit path
(`bpf_redirect_neigh(c->pool_ifindex, ...)` after Geneve decap); the
download direction leaked too because the pod's ACKs are transit packets.

### BPF cost per packet (`BPF_PROG_TEST_RUN`, the guard's method)

The guard's own `TestBPFNonVIPBudget` in the same VMs: 17/17, 22-23,
23-25, 28-34 ns (tcx4 / tcx6 / xdp4 / xdp6), matching its documented
17 / 23 / 23 / 30, so these compare directly. ns/packet, ranges over the
five kernels:

| Program, path | IPv4 | IPv6 |
|---|---|---|
| `pl_announce_all`, uplink miss | 39-40 | 40-42 |
| `pl_announce`, uplink miss | 34-35 | 36-37 |
| `pl_announce_all`, owner-local / no-owner drop | 52-54 | 53-57 |
| `pl_announce_all`, reply to tunnel | 68-75 | 69-77 |
| `pl_announce`, IPv6 Hop-by-Hop walk to tunnel | -- | 100-103 |
| `pl_announce`, first fragment / later fragment | 115-123 / 63-67 | 153-164 / 88-92 |
| `pl_announce`, PTB synthesis (per error) | 122-180 | 137-262 |
| `pl_tun_in`, reply-local (L2 / L3) | 67-77 / 64-67 | 68-77 / 66-70 |
| `pl_tun_in`, transit (L2 batched / L3 + Ethernet push) | 96-107 / 101-109 | 100-108 / 97-107 |
| `pl_steer`, slot to key, 2 announcers | 75-84 | 84-89 |
| `pl_pod_tag`, unmanaged device / unselected / selected | 12-13 / 22 / 22 | 12-13 / 22 / 22 |
| `pl_pod_out`, not selected | 40-42 | 40-42 |
| `pl_pod_reply`, uplink miss | 50-52 | 52-53 |
| `pl_pod_out`, established TCP / UDP | 172-177 / 166-168 | 161-171 / 154-165 |
| `pl_pod_reply`, established TCP | 132-145 | 128-137 |
| `pl_pod_out`, new flow (probe + 2 inserts) | 312-360 | 326-359 |

- The uplink miss is about 2x the guard; the prototype loads the L4 bytes
  before the `egress_addrs` lookup (reordering not measured).
- The eBPF stage's separate `pl_pod_reply` adds 50-53 ns to every uplink
  packet.
- Not exercisable in test-run: redirects actually taken, transit with real
  tunnel metadata.

### eBPF NAT filling one 4,096-port slice (16 linear probes; superseded below)

First failure at flow 1,767-2,431 (43-59% occupancy) on every kernel and
family; all 4,096 ports were mapped after 8,192 attempts (6.6 IPv6:
4,095). New-flow cost rises from about 370 ns to about 800 ns once probes
fail.

### eBPF NAT probing: 64 probes with an odd hash-derived step (2026-10-20, user decision)

Linear probing clusters, which is why 16 probes failed at about half full.
A simulation of one 4,096-port slice (200 trials each, first failure worst
/ median): linear 16 32.8% / 50.9% (matches the 43-59% measured), linear
64 60.4% / 76.6%, linear 128 71.4% / 84.5%; odd step 32 73.7% / 85.7%,
**odd step 64 87.5% / 93.3%** (mean 3.89 probes per new flow), odd step 128
94.3% / 97.3%. Implemented in natspike (`cand = start + ((h + i * ((h >> 16)
| 1)) & mask)`, `NAT_PROBES 64`) and measured on every kernel:

| Kernel | First failure IPv4 / IPv6 (of 4,096) | New flow, median ns (v4 / v6) | Attempt on a full slice, median ns (v4) |
|---|---|---|---|
| 6.6.15 | 3,848 / 3,905 | 353 / 346 | 2,740 |
| 6.8.0-146 | 3,770 / 3,867 | 359 / 365 | 2,642 |
| 6.12.111 | 3,745 / 3,792 | 344 / 348 | 2,551 |
| 6.17.0-1022 | 3,806 / 3,667 | 336 / 327.5 | 2,239 |
| 6.18.12 | 3,866 / 3,791 | 329 / 329 | 2,324 |

First failure at 89.5-95.3% occupancy, all 4,096 ports mapped; the new-flow
cost is unchanged; a full slice costs about 2.2-2.7 us per attempt (64
failed inserts) instead of about 0.8 us. Functional and load tests pass on
every kernel (6 / 6); `pl_pod_out` verifies at 2,049-2,890 instructions
(+4). Budget: eBPF first failure >= 85% of the slice, as for nft.

### Map memory (fdinfo memlock, bytes, same on every kernel within 0.1 KB)

| Map | Empty | Full |
|---|---|---|
| `nat` (131,072) | 2,098,048 | 18,875,264 |
| `slices` (65,536) | 1,049,472 | 6,816,640 |
| `frags` (LRU, preallocated) | 918,400 | 918,400 |
| `pod_devs` (4,096) | 66,432 | 426,880 |
| `egress_addrs` (1,024) | 17,280 | 107,392 |

### Throughput, CPU, rules, conntrack (netns over veth, relative)

- **Tunnel vs plain routing**, TCP medians: direct 0.76-0.93x (CPU/Gbit
  x1.00-1.32); L3 and L2 over IPv4 or IPv6 underlay 0.56-0.82x upload,
  0.64-0.81x download (CPU/Gbit x1.16-1.72). MTU 9000 is not exercised by
  veth (64 KB GSO regardless).
- **eBPF stage**: TCP within noise; 64-byte UDP +107 to +138 ns CPU per
  packet on every kernel.
- **nft per established flow** (spike 7's table vs a CNI-style conntrack +
  SNAT baseline): 64-byte UDP 0.70-0.86x, TCP 0.82-0.95x. Within-test
  ratios only: the nft test's plain mode cost about twice the same
  topology elsewhere, unexplained (idle CPU 0.000 before each window).
- **fwmark rules** (`bpf_fib_lookup`, mark 0, walks every rule): the first
  rule about +25 ns, then about 1.35 ns per rule (6.17: about 2.3); at 127
  rules 205-271 ns (IPv4), 245-304 ns (IPv6). Forwarded 64-byte UDP: +300 to
  +490 ns CPU per packet at 127 rules.
- **New flows until NAT failure** (one slice): nft maps all 4,096 ports,
  first miss at 88-96% occupancy, 144k-243k flows/s (CNI SNAT 148k-274k).
  Once the slice is full, 95k-109k attempts/s all fail and **no conntrack
  counter moves**, so exhaustion needs PureLB's own metric.
- **Conntrack**: one entry per flow, 249-259 B. Dump at 100k entries:
  count-only 36-42 ms, kernel mark filter 17.6-21.6 ms, parsed 152-190 ms,
  flush 19-25 ms.
- **Install**: 254 fwmark rules 0.8-3.4 ms; 65,536 `slices` entries
  7.2-9.0 ms batched (39.7-42.1 ms one by one); nft table 0.6-1.4 ms at
  16-32 slots.

### The nft table does not fit one default netlink transaction

With google/nftables v0.3.0 defaults, 64 slots or more fail on every kernel
(`netlink receive: recvmsg: no buffer space available`), and 127 slots x 2
families with `SendMessages: netlink send-messages: sendmsg: message too
long`; 16 and 32 slots succeed. The implementation must split the update
or raise the socket buffers.

**Raised buffers fix it (2026-10-21).** The same test with both netlink
socket buffers set through `nftables.WithSockOptions` (`SetReadBuffer` /
`SetWriteBuffer`, which use `SO_RCVBUFFORCE` / `SO_SNDBUFFORCE` and so need
CAP_NET_ADMIN, which the agent has) at 1, 4 and 16 MiB (`-nftbuf`, runner
part `nftbuf`): every case passes on every kernel, including 127 slots x 2
families, with no `message too long` or `no buffer space available`; 1 MiB is
enough. So the table stays one atomic transaction.

### Budgets (proposed by spike 10; approved by the user 2026-10-20; Verification item 4)

The eBPF NAT row was re-set after the probing change (64 probes, odd step; measured below).

Test-run budgets as a ratio to the guard measured in the same job (CI
runners are 4-6x slower); absolute numbers only on a reference machine.

| Gate | Proposed | Basis |
|---|---|---|
| Uplink fast path (miss) | <= 45 ns per family on the reference VM; in CI v4 <= 2.6x guard tcx4, v6 <= 2.0x guard tcx6; hits <= 60 (owner-local), <= 85 (to tunnel), <= 175 (first fragment) | 39-42 ns; ratios 2.29-2.35 / 1.74-1.91 |
| `pl_pod_tag` | <= 25 ns (<= 1.5x guard tcx4) | 22 ns |
| nft per established flow | small-packet rate >= 0.70x the CNI baseline, TCP >= 0.80x; re-measured on the real ruleset | 0.70-0.86 / 0.82-0.95 |
| Tunnel | >= 0.55x plain and CPU/Gbit <= 1.8x; direct >= 0.75x and <= 1.35x; eBPF stage <= +150 ns per small packet; no slab growth (<= 50 MB over a 10-minute soak) on every supported kernel | ratios above |
| D7 relay fraction | <= (k-1)/k + 0.10 with k advertisers; **proposed, unmeasured** | -- |
| New flows until NAT failure | nft and eBPF first failure >= 85% of the slice (eBPF: 64 probes, odd step), nft rate >= 0.5x CNI, eBPF new flow <= 400 ns; an exhaustion-drop metric | nft 88-96%; eBPF 89.5-95.3%; 327-365 ns |
| Conntrack | <= 300 B per flow; mark-filtered dump <= 0.3 ms per 1k entries | 256 B; 17.6-21.6 ms per 100k |
| API (kwok) | Set in Phase 3 from the real allocator; until then zero 429s and zero late lease renewals; kwok ranges only | spike 8 and its follow-ups; **Decided (user, 2026-10-21):** the numeric API budget is set from measurements of the real allocator in Phase 3 (a Phase 3 deliverable, before Phase 4), not from the kwok harness. Meanwhile the kwok numbers are ranges only: the harness runs in two unexplained modes about 2.4x apart (about 216 or about 500 egress writes/s under the decided allocator settings; kwok RESULTS, "Merge window"), and its writes come almost entirely from the claim path (about 30 slice-table versions per address against about 19 claims). Kept from the original budget: zero 429s and zero late lease renewals, which every run met. |
| CNI ADD p99 | active slice <= 100 ms; reserve claim <= 30 s at 500 nodes; **proposed, unmeasured** | spike 8 claim 12 / 25 s |
| RSS incl. BPF maps | <= baseline + 40 MiB at full maps; revisit the 100 Mi limit; **Go heap unmeasured** | maps 8.3 MB core + 18.9 MB NAT |
| Restart convergence | <= 2 s at maximum scale, excluding informer sync; **proposed, unmeasured** | parts sum to < 20 ms |

## Not yet run

- Spike 11: probe loss with BFD on, once k8gobgp's BFD recovers after a
  session drop.
- Spike 10: the D7 relay fraction, CNI ADD p99, agent RSS and restart
  convergence (need the implementation or real hardware).
- Spike 2: `bpf_redirect_peer` into the pod and `pl_pod_flow` (pod replies
  to inbound connections) in the eBPF stage; `pl_pod_reply` called from
  `pl_announce`; the extension-header walk in the eBPF stage's parser.
- PTB from `pl_announce`: rate limiting, GSO and CHECKSUM_PARTIAL skbs.
