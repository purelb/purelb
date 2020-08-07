# PureLB Commands

PureLB is built on the idea that each command should do one thing and
do it well. This simplifies each command but it means that you need to
decide which commands that you want to run based on your environment.

There are two types of commands: controllers and speakers.
Controllers run a single instance and handle per-cluster activities
like managing IP addresses. Speakers run in a daemon set, i.e., on
each node in the cluster, and manage per-node activities like setting
up the node operating system when a service is created.

The controllers are:

* [controller-pool](controller-pool) - manages a local pool of IP addresses
* [controller-acnodal](controller-acnodal) - works with Acnodal's Enterprise Gateway
* controller-netbox - allocates addresses from a Netbox IPAM system (TBD)

The speakers are:

* [speaker-local](speaker-local) - configures the local operating system
* [speaker-acnodal](speaker-acnodal) - works with Acnodal's Enterprise Gateway

Commands are typically a small ```main()``` method that uses [internal
packages](../internal) to do most of the work.

Each command is built into its own [Docker image](container_registry).
