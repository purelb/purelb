# MetalLB NetBox Controller POC

This is a proof of concept of a controller that works with MetalLB and
allocates IP addresses from NetBox.  It's far from production-ready
but demonstrates how such a thing might work.

[MetalLB](https://metallb.universe.tf) is a load-balancer
implementation for bare metal [Kubernetes](https://kubernetes.io)
clusters, using standard routing protocols.  It's capable of managing
a pre-allocated pool of IP addresses, but isn't able to request
addresses from an IPAM system.

[NetBox](https://netbox.readthedocs.io/en/stable/) is a popular IPAM
and DCIM system.

# HOWTO

This POC queries NetBox for IP addresses whose tenant is
"ipam-metallb-customer-exp" and whose status is "reserved" so you'll
need to add a few using the NetBox user interface.  When it allocates
an address it marks it as "active" so it won't re-allocate it.

The easiest way to run this POC is to first [install MetalLB](https://metallb.universe.tf/installation/)
and configure it to *not* allocate IP addresses.  You can use the
["auto-assign: false" configuration
flag](https://metallb.universe.tf/configuration/#controlling-automatic-address-allocation).

You'll also want to configure the speakers to announce anything without second-guessing, so define a pool whose range is 0.0.0.0-255.255.255.255.  For example:

```
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    peers:
    - peer-address: 192.168.1.30
      peer-asn: 65551
      my-asn: 65552
    address-pools:
    - name: default
      auto-assign: false
      protocol: bgp
      addresses:
      - 0.0.0.0-255.255.255.255
```

Create the netbox-client namespace.  This allows the netbox-client controller to run alongside the MetalLB controller.

```
$ kubectl apply -f ./manifests/namespace.yaml
```

Configure a k8s secret that contains the NetBox user token (which the
controller uses to authenticate with NetBox).

```
$ kubectl create secret generic -n netbox-client netbox-client --from-literal=user-token="your-netbox-user-token"
```
If you don't have a token you can create one using the NetBox
admin user interface.

Edit "manifests/netbox-controller.yaml" so NETBOX_BASE_URL points to
your installation of NetBox.  Deploy the POC controller to a k8s
cluster by applying the manifest.

```
$ kubectl apply -f manifests/netbox-controller.yaml
```

As an alternative, the controller can run locally and connect to a
remote cluster if you have a kubeconfig file:

```
$ NETBOX_USER_TOKEN=your-user-token NETBOX_BASE_URL=http://localhost:8000 go run ./controller/main.go ./controller/service.go -kubeconfig $KUBECONFIG -config-ns netbox-client
```

## Testing

You can deploy a simple "echo" web application using kubectl:

```
$ kubectl create deployment source-ip-app --image=k8s.gcr.io/echoserver:1.10
```

...and then expose the app:

```
$ kubectl expose deployment source-ip-app --name=source-service --port=80 --target-port=8080 --type=LoadBalancer
```

You should see the POC controller allocate an address and mark that
address as "active" in NetBox.  The controller assigns the address to
the service and the MetalLB speakers advertise it as if it had been
allocated by the MetalLB controller.
