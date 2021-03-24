# PureLB Commands

There are two types of commands: allocators and node agents.
Allocators run in a single pod and handle per-cluster activities like
managing IP addresses. Node agents run in a daemon set (i.e., on each
node in the cluster) and manage per-node activities like configuring
the node network when a service is created.

The allocator can allocate addresses from local pools and from an
Acnodal EPIC instance.

* [allocator](allocator)

The node agent configures the local operating system and works with Acnodal's Enterprise Gateway.

* [lbnodeagent](lbnodeagent)

Commands are typically a small ```main()``` method that uses [internal
packages](../internal) to do most of the work.

Each command is built into its own [Docker image](container_registry).
