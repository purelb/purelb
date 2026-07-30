# Dual-stack HTTP echo server for manual LoadBalancer / service-affinity testing.
#
# Binds [::]:8080 as a dual-stack socket (answers both IPv4 and IPv6) and, for
# every request, returns the pod/node that served it and the exact provenance of
# each address value (downward API vs TCP socket vs HTTP header). A per-pod
# request counter makes it easy to watch traffic land on the announcing node as
# a VIP moves. Requires no build -- it runs on a stock python image and is fed
# in via a ConfigMap (see README.md).

import http.server, socket, os, ipaddress, threading, signal

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

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        if self.path == "/healthz":            # readiness probe: not counted
            self.send_response(200); self.send_header("Content-Length", "3")
            self.end_headers(); self.wfile.write(b"ok\n"); return
        n = next_count()
        peer = self.connection.getpeername()   # kernel: packet SOURCE addr/port
        loc  = self.connection.getsockname()   # kernel: local (pod-side) addr/port
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
          "http_host            = {:<28} [Host header = the VIP:port the client dialed]".format(self.headers.get("Host", "<none>")),
          "http_x_forwarded_for = {:<28} [only present if an L7 proxy added it]".format(self.headers.get("X-Forwarded-For", "<none>")),
          "http_x_real_ip       = {:<28} [only present if an L7 proxy added it]".format(self.headers.get("X-Real-IP", "<none>")),
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
