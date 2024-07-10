# PureLB - is a Service Load Balancer for Kubernetes

[PureLB](https://purelb.io) is a load-balancer orchestrator for  [Kubernetes](https://kubernetes.io) clusters. It uses standard
Linux networking and routing protocols,  and works with the operating
system to announce service addresses.

## Documentation

https://purelb.gitlab.io/purelb/

## Quick Start

Installation is easy. For production systems we recommend installing using either Helm or a CI-built manifest file. These approaches use versioned image tags so they are stable. Instructions are on our install page at https://purelb.gitlab.io/purelb/install/ .

For development, you can install PureLB from the source tree. This isn't recommended for production because it will install PureLB using an unstable image tag that changes over time so you could have unintended upgrades.

1. Deploy the PureLB components<br/>
`kustomize build deployments/samples | kubectl apply -f -`<br/>
Now you can configure PureLB. PureLB's default node agent
configuration usually "just works" so we loaded it above.  PureLB's
allocator manages IP addresses so it needs a configuration that
matches the network on which it's running.  The allocator is
configured using "ServiceGroup" resources which contain pools of IP
addresses and their associated network configuration.  The node agent
configures the Linux OS to advertise them.  The easiest way to get
started is to create a ServiceGroup that uses the same IPNET as the
host interface, PureLB will add the allocated addresses to the same
network interface.
1. Copy the default ServiceGroup config to your custom version<br/>
`cp configs/default-servicegroup.yaml configs/my-servicegroup.yaml`
1. Edit `configs/my-servicegroup.yaml` so the `subnet` and `pool` are appropriate for your network. The sample assumes a v4 pool but either v4, v6, or both can be configured
1. Load your ServiceGroup config<br/>
`kubectl apply -f configs/my-servicegroup.yaml`

To test PureLB you can deploy a simple "echo" web application:

```shell
kubectl create deployment echoserver --image=k8s.gcr.io/echoserver:1.10
```

...and then expose the deployment using a LoadBalancer service:

```shell
kubectl expose deployment echoserver --name=echoserver-service --port=80 --target-port=8080 --type=LoadBalancer
```

The PureLB allocator will allocate one or more addresses and assign them to the
service. The PureLB node agents then configure the underlying
operating system to advertise the addresses.

## Building

Run `make help` for Makefile documentation.

If you fork this project and want to build ARM images you'll need to set up a Gitlab CI ARM "runner" process for your fork. I use a Raspberry Pi 4 Model B running Raspbian GNU/Linux 10 (Buster).

Find your project's registration token from the project CI Settings - https://gitlab.com/{your_gitlab_name}/purelb/-/settings/ci_cd . Expand the "Runners" section and look for "Set up a specific Runner manually".

Install the runner client program - https://docs.gitlab.com/runner/install/linux-repository.html

Run `gitlab-runner register`. It will ask for the url (`https://gitlab.com/`) and registration token.

Stop the runner (`systemctl stop gitlab-runner.service`) and make a few edits to the `/etc/gitlab-runner/config.toml` runner config file:

* Set the `privileged` flag to `true` (it's `false` by default)
* Add `"/certs/client"` to the `volumes` list
* Increase `wait_for_services_timeout` to 300
* If you're running a Pi4 with 4GB of memory or more you can set `concurrent` to 2 so the allocator and lbnodeagent will build simultaneously

Start the runner `systemctl start gitlab-runner.service` and verify that it's running `systemctl status gitlab-runner.service`.

Verify that the runner is listed on your project's CI Settings page.

## Code

* [Commands](cmd) - if you're a "top-down" learner then start here
* [Internal Code](internal) - if you're a "bottom-up" learner then start here
* [Docker Packaging](build/package)
* [Helm Packaging](build/helm)
* [Sample Configurations](configs)
* [K8s Deployment Files](deployments)

## Credits

PureLB wouldn't have been possible without MetalLB so we owe a huge
debt of gratitude to [Dave Anderson](https://www.dave.tf/) for almost
single-handedly making MetalLB happen. Thank you Dave!
