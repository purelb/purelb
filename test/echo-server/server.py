# Dual-stack HTTP echo server for manual LoadBalancer / service-affinity testing.
#
# Binds [::]:8080 as a dual-stack socket (answers both IPv4 and IPv6) and, for
# every request, returns the pod/node that served it and the exact provenance of
# each address value (downward API vs TCP socket vs HTTP header). A per-pod
# request counter makes it easy to watch traffic land on the announcing node as
# a VIP moves. Requires no build -- it runs on a stock python image and is fed
# in via a ConfigMap (see README.md).

import http.server, socket, os, ipaddress, threading, signal
import json, urllib.parse

# Exit immediately on SIGTERM so a rescheduled/deleted pod terminates fast
# (default is to run until SIGKILL after terminationGracePeriodSeconds). This
# keeps the old pod's endpoint from lingering as "serving" during a move, so
# service-affinity converges the VIP to the new node quickly.
signal.signal(signal.SIGTERM, lambda *_: os._exit(0))

# In-memory request counter for THIS pod process (resets on restart/reschedule).
# Locked because the server is multi-threaded.
_lock = threading.Lock()
_count = 0
def next_count():
    global _count
    with _lock:
        _count += 1
        return _count

# Kubernetes downward API -> container env (see the Deployment `env:` block).
POD    = os.environ.get("POD_NAME", "<unset>")   # fieldRef: metadata.name
NODE   = os.environ.get("NODE_NAME", "<unset>")  # fieldRef: spec.nodeName
NODEIP = os.environ.get("HOST_IP", "<unset>")    # fieldRef: status.hostIP
PODIP  = os.environ.get("POD_IP", "<unset>")     # fieldRef: status.podIP

def unmap(raw):
    a = ipaddress.ip_address(raw)
    return str(a.ipv4_mapped) if (a.version == 6 and a.ipv4_mapped) else str(a)

def hostport(raw, port):
    a = unmap(raw)
    return ("[{}]:{}" if ipaddress.ip_address(a).version == 6 else "{}:{}").format(a, port)

def fam(raw):
    a = ipaddress.ip_address(raw)
    if a.version == 6 and a.ipv4_mapped:
        return "IPv4 (v4-mapped on a v6 socket)"
    return "IPv{}".format(a.version)

def header_or(h, name):
    """Header value, or None. Text renders None as <none>; JSON as null."""
    v = h.get(name)
    return v if v else None


def collect(handler, n, peer, loc):
    """Everything the response reports, gathered once.

    Both renderings read from this, so the text a human curls and the
    JSON a test parses can never disagree about what was observed.
    """
    praw, lraw = peer[0], loc[0]
    pa = ipaddress.ip_address(praw)
    return {
        "request_count": n,
        "pod": POD, "node": NODE, "node_ip": NODEIP, "pod_ip": PODIP,
        "tcp_peer_src":  {"ip": unmap(praw), "port": peer[1]},
        "tcp_local_dst": {"ip": unmap(lraw), "port": loc[1]},
        # Split apart deliberately: the text form says "IPv4 (v4-mapped on
        # a v6 socket)", and a test asserting the address family should not
        # have to parse prose to get it.
        "socket_family": "IPv4" if (pa.version == 4 or pa.ipv4_mapped) else "IPv6",
        "ipv4_mapped": bool(pa.version == 6 and pa.ipv4_mapped),
        "http_host": header_or(handler.headers, "Host"),
        "http_x_forwarded_for": header_or(handler.headers, "X-Forwarded-For"),
        "http_x_real_ip": header_or(handler.headers, "X-Real-IP"),
        "http_path": handler.path,
    }


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def wants_json(self):
        """Accept: application/json, or ?format=json.

        Two ways on purpose. The harness sets the header, which is the
        correct HTTP thing; ?format=json is for poking at it from a node
        shell, where adding a header is more to type than it is worth.
        Text stays the default so the manual workflow is unchanged.
        """
        if "application/json" in (self.headers.get("Accept") or ""):
            return True
        query = urllib.parse.urlparse(self.path).query
        return urllib.parse.parse_qs(query).get("format", [""])[0] == "json"

    def do_GET(self):
        # Compare the parsed path, not the raw one, so /healthz?x=y is
        # still the probe rather than a counted request.
        if urllib.parse.urlparse(self.path).path == "/healthz":
            self.send_response(200); self.send_header("Content-Length", "3")
            self.end_headers(); self.wfile.write(b"ok\n"); return
        n = next_count()
        peer = self.connection.getpeername()   # kernel: packet SOURCE addr/port
        loc  = self.connection.getsockname()   # kernel: local (pod-side) addr/port
        f = collect(self, n, peer, loc)

        if self.wants_json():
            body = (json.dumps(f, indent=2) + "\n").encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        lines = [
          "# source: this pod process (in-memory, resets on pod restart/reschedule)",
          "request_count        = {:<28} [# requests served by THIS pod instance]".format(n),
          "",
          "# source: Kubernetes downward API (pod env)",
          "pod                  = {:<28} [env POD_NAME  <- fieldRef metadata.name]".format(POD),
          "node                 = {:<28} [env NODE_NAME <- fieldRef spec.nodeName]".format(NODE),
          "node_ip              = {:<28} [env HOST_IP   <- fieldRef status.hostIP]".format(NODEIP),
          "pod_ip               = {:<28} [env POD_IP    <- fieldRef status.podIP]".format(PODIP),
          "",
          "# source: TCP socket (kernel) -- this is where the 'client' value comes from",
          "tcp_peer_src         = {:<28} [socket.getpeername() = packet SOURCE IP; ETP Cluster SNATs it to the node]".format(hostport(peer[0], peer[1])),
          "tcp_local_dst        = {:<28} [socket.getsockname() = local addr; kube-proxy DNAT'd the VIP -> pod_ip]".format(hostport(loc[0], loc[1])),
          "socket_family        = {:<28} [address family of the accepted connection]".format(fam(peer[0])),
          "",
          "# source: HTTP request headers (what the client wrote at L7)",
          "http_host            = {:<28} [Host header = the VIP:port the client dialed]".format(f["http_host"] or "<none>"),
          "http_x_forwarded_for = {:<28} [only present if an L7 proxy added it]".format(f["http_x_forwarded_for"] or "<none>"),
          "http_x_real_ip       = {:<28} [only present if an L7 proxy added it]".format(f["http_x_real_ip"] or "<none>"),
          "http_path            = {:<28} [request line]".format(self.path),
          "",
        ]
        body = ("\n".join(lines)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass

class Server(http.server.ThreadingHTTPServer):
    address_family = socket.AF_INET6           # dual-stack socket
    def server_bind(self):
        self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        super().server_bind()

print("echo-server listening on [::]:8080 (dual-stack)", flush=True)
Server(("::", 8080), H).serve_forever()
