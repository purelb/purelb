# natspike: completing spike 2

Spike code, not production code. Two gaps left by spike 2 (see
`../../RESULTS.md`), closed on every kernel of the matrix:

1. **PureLB-owned eBPF NAT for the pod-node eBPF stage (D32, design §6.4).**
   `bpf/egress_nat.c`:
   - `pl_pod_out` (tcx egress on the host NIC, after routing, as design
     §6.4 places it; the pod is found by `skb->ingress_ifindex`, its
     host-side veth) translates the pod's TCP, UDP and ICMP/ICMPv6 echo to the egress
     address and a port (or echo identifier) in the node's slice
     `[start, start+size)`. A new flow probes at most `NAT_PROBES` (64; 16 until 2026-10-20)
     candidates, starting at the flow hash's low bits and stepping by its
     high bits made odd (double hashing; linear probing clustered and
     failed at about half full); inserting the translated
     tuple with `BPF_NOEXIST` is the claim on the port. One `nat` HASH map
     (`BPF_F_NO_PREALLOC`) holds the translated-tuple entry (original
     address and port, TCP-aware state, last-seen time) and a reverse entry
     keyed by the original tuple. IPv4 header and L4 checksums (with the
     pseudo header for TCP, UDP and ICMPv6) are fixed with the checksum
     helpers.
   - `pl_pod_reply` (tcx ingress on the uplink here) reverses replies, and
     ICMP/ICMPv6 errors whose quoted header carries the translated tuple:
     outer destination, quoted source address and port (or echo id), quoted
     IPv4 header checksum, quoted L4 checksum, and the ICMP checksum (plus
     the ICMPv6 pseudo header).
   - Fragments are dropped and counted (`nat_frag_drop`), at both programs.
   - GC is user space (`gcNat` in `nat_test.go`): per-state idle timeouts,
     closing TCP 10 s.
2. **One `pl_announce` for both roles.** `pl_announce_all` takes the uplink
   path, or the tunnel-ingress path (the spike's `pl_tun_in`) when
   `skb->ifindex` is the configured tunnel, so both are verified as one
   program.

## Run

```
SPIKES_PKG=natspike hack/egress-spikes/vm/vm-test.sh
```

`TestSpike2LoadNat` runs with only CAP_BPF + CAP_NET_ADMIN (the agent's
set) and prints each program's verified instruction count, or the verifier
log on failure. `TestSpike2NatPath` builds pod -- node -- server namespaces
and prints one `SPIKE2NAT RESULT` line per case and family: TCP, UDP, ICMP
echo, ICMP error (UDP to a closed port: the pod's socket must get
ECONNREFUSED), fragments, conntrack untouched, GC.

Regenerate the object after editing the C (from the repository root):

```
docker run --rm --user $(id -u):$(id -g) -e HOME=/tmp -e GOCACHE=/tmp/gocache \
  -e GOTOOLCHAIN=local -e GOMODCACHE=/gomodcache -v $(go env GOMODCACHE):/gomodcache \
  -v $PWD:/src -w /src/hack/egress-spikes/vm/natspike purelb-bpf-builder:local go generate ./...
```

## Shapes the verifier forced (no CAP_PERFMON)

- **Select values with a mask, not a ternary.** `v6 ? pn->eg6[0] : pn->eg4`
  became a select between two map-value addresses ("tried to add from
  different maps, paths or scalars"); `pick()` loads both and masks.
- **Port probing runs in `bpf_loop`.** As a plain bounded loop each early
  exit carried the rest of the program as a separate path: 6.12 refused it
  ("The sequence of 8193 jumps is too complex"), 6.6 and 6.8 failed with
  EFAULT; 6.17 and 6.18 accepted it. The callback is verified once.

## Scope notes

- `pl_pod_out` sits on the host NIC's tcx egress (design §6.4), so the
  translation follows the routing decision and no `accept_local` is
  needed (the test checks it is 0). `TestSpike2NatPath/veth-ingress-control`
  attaches the same program on the pod veth's tcx ingress instead, where
  IPv4 forwarding drops the translated packet as a martian source unless
  `accept_local=1`; IPv6 needs nothing there either.
- `pl_pod_reply` is a separate program on the uplink's tcx ingress, the
  hook where §6.4 calls it from `pl_announce`; it returns the de-NATed
  packet to the stack rather than `bpf_redirect_peer` into the pod.
- IPv6 extension headers other than the fragment header are not walked.
- Not covered: MSS clamping, PMTU/frag-needed synthesis, tunnel steering
  from `pl_pod_out`, and `pl_announce_all` traffic (it is load-tested; its
  two halves were exercised as separate programs in spike 1).
