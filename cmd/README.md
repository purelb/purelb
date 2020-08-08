# PureLB Commands

PureLB is built on the idea that each command should do one thing and
do it well. This simplifies each command but it means that you need to
decide which commands that you want to run based on your environment.

There are two types of commands: allocators and nodes.  Allocators run
in a single pod and handle per-cluster activities like managing IP
addresses. Nodes run in a daemon set, i.e., on each node in the
cluster, and manage per-node activities like setting up the node
operating system when a service is created.

The allocators are:

* [allocator-local](allocator-local) - allocates addresses from a local pool
* [allocator-acnodal](allocator-acnodal) - works with Acnodal's Enterprise Gateway
* allocator-netbox - allocates addresses from a Netbox IPAM system (TBD)

The nodes are:

* [node-local](node-local) - configures the local operating system
* [node-acnodal](node-acnodal) - works with Acnodal's Enterprise Gateway

Commands are typically a small ```main()``` method that uses [internal
packages](../internal) to do most of the work.

Each command is built into its own [Docker image](container_registry).
