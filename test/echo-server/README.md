# echo-server — dual-stack test container

A tiny HTTP echo server for manually testing LoadBalancer announcement and
service affinity. It answers on **both IPv4 and IPv6**, and every response tells
you **which pod and node served the request** plus the exact provenance of each
address value. A per-pod request counter makes it obvious when traffic follows a
VIP to a new node.

No image to build — it runs the code in [`server.py`](server.py) on a stock
`python:3.12-alpine` image, mounted via a ConfigMap.

## Deploy

```sh
kubectl create namespace echo-test
kubectl create configmap echo-server -n echo-test --from-file=server.py
kubectl apply -f deploy.yaml
```

Prerequisite: a **local** ServiceGroup that `deploy.yaml` references by name
(`purelb.io/service-group: default`). Change that annotation to match a local
ServiceGroup on your cluster if yours isn't called `default`.

Get the VIP(s):

```sh
kubectl get svc echo -n echo-test -o wide
V4=$(kubectl get svc echo -n echo-test -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
V6=$(kubectl get svc echo -n echo-test -o jsonpath='{.status.loadBalancer.ingress[1].ip}')
```

## Query it

Curl from anywhere that can reach the VIP (e.g. SSH to a cluster node):

```sh
curl http://$V4/
curl -g "http://[$V6]/"        # -g so curl doesn't treat [] as a glob
```

Example response:

```
request_count        = 3                            [# requests served by THIS pod instance]
pod                  = echo-5967b68d75-lqxbj        [env POD_NAME  <- fieldRef metadata.name]
node                 = purelb2-1                    [env NODE_NAME <- fieldRef spec.nodeName]
node_ip              = 172.30.250.104               [env HOST_IP   <- fieldRef status.hostIP]
pod_ip               = 172.24.0.139                 [env POD_IP    <- fieldRef status.podIP]
tcp_peer_src         = 172.30.250.100:23625         [socket.getpeername() = packet SOURCE IP; ...]
tcp_local_dst        = 172.24.0.139:8080            [socket.getsockname() = local addr; ...]
socket_family        = IPv4 (v4-mapped on a v6 socket)
http_host            = 172.30.250.150               [Host header = the VIP:port the client dialed]
http_x_forwarded_for = <none>
http_x_real_ip       = <none>
```

Each line names where the value comes from. Notes for `externalTrafficPolicy:
Cluster` (the only mode local pools support): kube-proxy **DNAT**s the VIP to the
pod, so `tcp_local_dst` is the pod IP (not the VIP — that's in `http_host`), and
it **SNAT**s the source, so `tcp_peer_src` is the node IP, not the real caller.
`node`/`node_ip` are always the pod's node. Health probes hit `/healthz` and are
not counted.

## Watch service affinity move the VIP

Pin the pod to a node, then move it and watch the VIP + counter follow. Only
nodes on the VIP's subnet are eligible.

```sh
# pin to a node
kubectl -n echo-test patch deployment echo --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<nodeA>"}}}}}'

# in another terminal, poll the VIP once a second (curl from a node that can reach it)
watch -n1 "ssh <node-ip> \"curl -s http://$V4/ | grep -E 'request_count|node '\""

# move the pod -> the pod reschedules, the VIP follows, the new pod's counter starts at 1
kubectl -n echo-test patch deployment echo --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<nodeB>"}}}}}'
```

Cross-check what PureLB thinks against the interfaces:

```sh
kubectl get svc echo -n echo-test -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv4}{"\n"}'
ssh <node-ip> "ip -o addr show | grep $V4"      # ground truth
```

## IPv4-only or IPv6-only

Edit `deploy.yaml`: set `ipFamilyPolicy: SingleStack` and `ipFamilies: [ IPv4 ]`
(or `[ IPv6 ]`). The server binds dual-stack regardless; the Service decides
which family(ies) get a VIP.

## Update the code

`server.py` is the source of truth. After editing it, refresh the ConfigMap and
restart the pod:

```sh
kubectl create configmap echo-server -n echo-test --from-file=server.py \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n echo-test rollout restart deployment/echo
```

## Clean up

```sh
kubectl delete namespace echo-test
```
