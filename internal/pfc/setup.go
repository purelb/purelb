package pfc

import (
	"fmt"
	"os/exec"

	"github.com/go-kit/kit/log"
)

// runProgram is a little helper that runs a program and logs what's
// being run. If the result is non-zero then it logs the error, too.
func runProgram(log log.Logger, program string, args ...string) error {
	cmd := exec.Command(program, args...)
	err := cmd.Run()
	if err != nil {
		log.Log("command", cmd.String(), "error", err)
	} else {
		log.Log("command", cmd.String())
	}
	return err
}

// runScript is a little helper that runs a shell script and logs
// what's being run. If the result is non-zero then it logs the error,
// too.
func runScript(log log.Logger, script string) error {
	return runProgram(log, "/bin/sh", "-c", script)
}

// CleanupQueueDiscipline removes our qdisc from the specified nic. It's useful
// for forcing SetBalancer to reload the filters which also
// initializes the maps.
func CleanupQueueDiscipline(log log.Logger, nic string) error {
	// tc filter del dev cni0 egress
	return runProgram(log, "/usr/sbin/tc", "qdisc", "del", "dev", nic, "clsact")
}

// CleanupFilter removes our filters from the specified nic. It's useful
// for forcing SetBalancer to reload the filters which also
// initializes the maps.
func CleanupFilter(log log.Logger, nic string, direction string) error {
	// tc filter del dev cni0 egress
	return runProgram(log, "/usr/sbin/tc", "filter", "del", "dev", nic, direction)
}

// SetupNIC adds the PFC components to nic. direction should be either
// "ingress" or "egress". qid should be 0 or 1. flags is typically
// either 8 or 9 where 9 adds debug logging.
func SetupNIC(log log.Logger, nic string, direction string, qid int, flags int) error {

	// tc qdisc add dev nic clsact
	addQueueDiscipline(log, nic)

	// tc filter add dev nic ingress bpf direct-action object-file pfc_ingress_tc.o sec .text
	addFilter(log, nic, direction)

	// ./cli_cfg set nic 0 0 9 "nic rx"
	configurePFC(log, nic, qid, flags)

	return nil
}

func addQueueDiscipline(log log.Logger, nic string) error {
	// add the clsact qdisc to the nic if it's not there
	return runScript(log, fmt.Sprintf("/usr/sbin/tc qdisc list dev %[1]s clsact | /usr/bin/grep clsact || /usr/sbin/tc qdisc add dev %[1]s clsact", nic))
}

func addFilter(log log.Logger, nic string, direction string) error {
	// add the pfc_{ingress|egress}_tc filter to the nic if it's not already there
	return runScript(log, fmt.Sprintf("/usr/sbin/tc filter show dev %[1]s %[2]s | /usr/bin/grep pfc_%[2]s_tc || /usr/sbin/tc filter add dev %[1]s %[2]s bpf direct-action object-file /opt/acnodal/bin/pfc_%[2]s_tc.o sec .text", nic, direction))
}

func configurePFC(log log.Logger, nic string, qid int, flags int) error {
	return runScript(log, fmt.Sprintf("/opt/acnodal/bin/cli_cfg set %[1]s %[2]d %[3]d \"%[1]s rx\"", nic, qid, flags))
}

// SetTunnel sets the parameters needed by one PFC tunnel.
func SetTunnel(log log.Logger, tunnelID uint32, tunnelAddr string, myAddr string, tunnelPort int32) error {
	return runScript(log, fmt.Sprintf("/opt/acnodal/bin/cli_tunnel get %[1]d || /opt/acnodal/bin/cli_tunnel set %[1]d %[3]s %[4]d %[2]s %[4]d", tunnelID, tunnelAddr, myAddr, tunnelPort))
}

// SetService sets the parameters needed by one PFC service.
func SetService(log log.Logger, groupId uint16, serviceId uint16, tunnelAuth string, tunnelID uint32) error {
	return runScript(log, fmt.Sprintf("/opt/acnodal/bin/cli_service set-node %[1]d %[2]d %[3]s %[4]d", groupId, serviceId, tunnelAuth, tunnelID))
}
