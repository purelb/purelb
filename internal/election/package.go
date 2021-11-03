/*
   Package election manages the process of deciding to which node each
   local IP address is assigned. PureLB works by assigning local LB IP
   addresses to a network interface on exactly one node, which causes
   Linux on that node to respond to ARP requests and attract traffic
   for that address.

   Reliable operation depends on each IP address always being on one
   and only one node at a time. This is where the memberlist package
   comes into play. Kubernetes' native mechanisms for detecting pod
   and node failure have higher latency than is appropriate for this
   use case, so we use memberlist[1] to quickly decide which node
   hosts each LB address. Memberlist works by holding an election
   among the participating nodes; the winner of the election adds the
   LB address to its network interface. If that node becomes
   unavailable then memberlist re-runs the election and chooses a new
   winner.

   [1] https://github.com/hashicorp/memberlist

*/

package election
