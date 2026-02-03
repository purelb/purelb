/*
Package election manages the process of deciding to which node each
local IP address is assigned. PureLB works by assigning local LB IP
addresses to a network interface on exactly one node, which causes
Linux on that node to respond to ARP requests and attract traffic
for that address.

Reliable operation depends on each IP address always being on one
and only one node at a time. This package implements subnet-aware
leader election using Kubernetes Leases to decide which node hosts
each LB address.

# How It Works

Each lbnodeagent creates a Lease object in its namespace containing:
  - The node name (as holder identity)
  - The subnets available on that node (as an annotation)
  - A renewal timestamp that gets updated periodically

When determining which node should announce a given IP address, the
election system:

 1. Finds all nodes with valid (non-expired) leases
 2. Filters to nodes that have the IP's subnet in their annotations
 3. Uses a deterministic hash of (node name + service key) to pick a winner

This ensures that:
  - Only nodes on the same subnet as the VIP can announce it
  - The same node wins consistently for a given service (stable ownership)
  - Failover happens automatically when a node's lease expires

# Graceful Shutdown

During shutdown, a node:
 1. Marks itself unhealthy (Winner() returns "" for all queries)
 2. Triggers a ForceSync to withdraw all addresses
 3. Deletes its lease so other nodes see it gone immediately

This enables zero-downtime rolling updates when combined with
terminationGracePeriodSeconds.
*/
package election
