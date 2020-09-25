# PureLB - is a Service Load Balancer for Kubernetes

[PureLB](https://purelb.io) is a load-balancer orchestrator for  [Kubernetes](https://kubernetes.io) clusters. It uses standard
Linux networking and routing protocols,  and works with the operating
system to announce service addresses.

## Documentation

https://purelb.gitlab.io/docs

## Quick Start

PureLB uses Service Groups which contain pools of IP addresses and their associated network configuration.  It configures the Linux OS to
advertise them.  The easiest way to get started is to create a Service Group that uses the same IPNET as the host interface, PureLB will add
the allocated addresses to the same network interface.  Installation takes only a few steps:

1. Create the PureLB namespace<br/>
`kubectl apply -f deployments/namespace.yaml`
1. Create the PureLB custom resource definitions<br/>
`kubectl apply -f deployments/crds/lbnodeagent.purelb.io_crd.yaml -f deployments/crds/servicegroup.purelb.io_crd.yaml`
1. Load a sample PureLB configuration<br/>
`kubectl apply -f configs/default-lbnodeagent.yaml -f configs/default-servicegroup.yaml`
1. Deploy the PureLB components<br/>
`kubectl apply -f deployments/purelb.yaml`

You can deploy a simple "echo" web application with a single command:

```shell
kubectl create deployment echoserver --image=k8s.gcr.io/echoserver:1.10
```

...and then expose the deployment using a LoadBalancer service:

```shell
kubectl expose deployment echoserver --name=echoserver-service --port=80 --target-port=8080 --type=LoadBalancer
```

The PureLB allocator will allocate an address and assign it to the
service. The PureLB node agents then configure the underlying
operating system to advertise the address.

## Building

Run `make help` for Makefile documentation.

## Code

* [Commands](cmd) - if you're a "top-down" learner then start here
* [Internal Code](internal) - if you're a "bottom-up" learner then start here
* [Packaging](build/package)
* [Sample Configurations](configs)
* [K8s Deployment Files](deployments)

## Credits

PureLB wouldn't have been possible without MetalLB so we owe a huge
debt of gratitude to [Dave Anderson](https://www.dave.tf/) for almost
single-handedly making MetalLB happen. Thank you Dave!
