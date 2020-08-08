# PureLB - a bare-metal load balancer for Kubernetes

[PureLB](https://purelb.io) is a load-balancer implementation for bare
metal [Kubernetes](https://kubernetes.io) clusters. It uses standard
routing protocols, runs on Linux systems, and works with the operating
system to announce service addresses.

## Quick Start

The easiest way to get started is to use what we call a "local"
configuration. PureLB will manage a pool of IP addresses and will
configure the underlying Linux OS to advertise them.  Installation is
a three-step process:

1. Create the PureLB namespace<br/>
`kubectl apply -f deployments/namespace.yaml`
1. Load a sample PureLB configuration<br/>
`kubectl apply -f configs/sample-configmap-local.yaml`
1. Deploy the PureLB components<br/>
`kubectl apply -f deployments/purelb-local.yaml`

## Testing

You can deploy a simple "echo" web application with a single command:

```shell
kubectl create deployment echoserver --image=k8s.gcr.io/echoserver:1.10
```

...and then expose the deployment using a service:

```shell
kubectl expose deployment echoserver --name=echoserver-service --port=80 --target-port=8080 --type=LoadBalancer
```

You should see the PureLB allocator allocate an address and assign it
to the service. The PureLB nodes then configure the underlying
operating system to advertise the address.

## Troubleshooting

FIXME how to find logs

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
