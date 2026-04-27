---
title: "Your First LoadBalancer Service"
description: "End-to-end walkthrough: create a ServiceGroup, deploy a workload, and expose it with a LoadBalancer."
weight: 40
---

After installing PureLB, follow these steps to create your first working LoadBalancer Service.

## Step 1: Create a ServiceGroup

A ServiceGroup tells PureLB which IP addresses it can allocate. You need at least one ServiceGroup before PureLB can assign addresses to services.

Copy the example below and **edit the `subnet` and `pool` values** to match a free range on your network:

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24            # Your LAN subnet
      pool: 192.168.1.240-192.168.1.250  # Free range in that subnet
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120       # Your IPv6 subnet (omit on IPv4-only clusters)
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
```

> [!NOTE]
> The `subnet` must be the network that contains your pool addresses. The `pool` is the specific range PureLB will allocate from. These addresses must be free on your network -- PureLB does not check for conflicts.

Save this as `servicegroup.yaml` and apply it:

```sh
kubectl apply -f servicegroup.yaml
```

PureLB uses the ServiceGroup named `default` when no `purelb.io/service-group` annotation is specified on a Service.

## Step 2: Deploy a Workload

Deploy nginx as a test backend:

```sh
kubectl create deployment nginx --image=nginx
```

## Step 3: Create a LoadBalancer Service

Create a dual-stack LoadBalancer Service that targets the nginx pods:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nginx
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  ipFamilyPolicy: PreferDualStack
  ipFamilies:
  - IPv4
  - IPv6
  selector:
    app: nginx
  ports:
  - name: http
    port: 80
    targetPort: 80
```

Apply it:

```sh
kubectl apply -f https://raw.githubusercontent.com/purelb/purelb/main/deployments/samples/sample-nginx-lb.yaml
```

`ipFamilyPolicy: PreferDualStack` works unchanged on both single-stack and dual-stack clusters.

## Step 4: Verify

Check the service status:

```sh
kubectl get svc nginx
```

You should see one or two addresses in the `EXTERNAL-IP` column:

```plaintext
NAME    TYPE           CLUSTER-IP    EXTERNAL-IP                           PORT(S)        AGE
nginx   LoadBalancer   10.96.45.12   192.168.1.240,fd53:9ef0:8683::1      80:31234/TCP   10s
```

Test connectivity from a host with a route to the pool subnet:

```sh
curl http://192.168.1.240
curl -6 http://[fd53:9ef0:8683::1]
```

## Step 5: Inspect with kubectl-purelb

If you have the [kubectl-purelb plugin]({{< relref "/docs/operations/kubectl-plugin" >}}) installed:

```sh
kubectl purelb status
kubectl purelb pools
```

These commands show pool utilization, election state, and service announcements.

## What Happened

PureLB's [data flow]({{< relref "/docs/overview/architecture#data-flow" >}}) in action: the Allocator allocated addresses from the `default` ServiceGroup, the election system chose an announcing node, and that node added the addresses to its physical interface. kube-proxy independently configured nftables rules to forward traffic to the nginx pods.
