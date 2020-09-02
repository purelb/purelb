# PureLB Commands

PureLB is built on the idea that each command should do one thing and
do it well. This simplifies each command but it means that you need to
decide which commands that you want to run based on your environment.

There are two types of commands: allocators and node agents.
Allocators run in a single pod and handle per-cluster activities like
managing IP addresses. Node agents run in a daemon set (i.e., on each
node in the cluster) and manage per-node activities like configuring
the node network when a service is created.

There's only one allocator command. It can allocate addresses from
local pools and from an Acnodal EGW instance.

* [allocator](allocator) - allocates addresses from local and remote pools

The node agents are:

* [lbnodeagent-local](lbnodeagent-local) - configures the local operating system
* [lbnodeagent-acnodal](lbnodeagent-acnodal) - works with Acnodal's Enterprise Gateway

Commands are typically a small ```main()``` method that uses [internal
packages](../internal) to do most of the work.

Each command is built into its own [Docker image](container_registry).
