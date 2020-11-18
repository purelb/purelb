package pfc

import (
	"fmt"
	"os/exec"

	"github.com/go-kit/kit/log"
)

// SetupNIC adds the PFC components to nic. direction should be either
// "ingress" or "egress". qid should be 0 or 1. flags is typically
// either 8 or 9 where 9 adds debug logging.
func SetupNIC(log log.Logger, nic string, direction string, qid int, flags int) error {
	var err error

	// tc qdisc add dev nic clsact
	err = addQueueDiscipline(log, nic)
	if err == nil {
		log.Log(nic, "qdisc added")
	} else {
		log.Log(err, "qdisc add error")
	}

	// tc filter add dev nic ingress bpf direct-action object-file pfc_ingress_tc.o sec .text
	err = addFilter(log, nic, direction)
	if err == nil {
		log.Log(nic, "filter added", "direction", direction)
	} else {
		log.Log(err, "filter add error")
	}

	// ./cli_cfg set nic 0 0 9 "nic rx"
	err = configurePFC(log, nic, qid, flags)
	if err == nil {
		log.Log(nic, "pfc configured", "qid", qid, "flags", flags)
	} else {
		log.Log(err, "pfc configuration error")
	}

	return nil
}

func addQueueDiscipline(log log.Logger, nic string) error {
	// add the clsact qdisc to the nic if it's not there
	script := fmt.Sprintf("tc qdisc list dev %[1]s clsact | grep clsact || tc qdisc add dev %[1]s clsact", nic)
	log.Log("script", script)
	cmd := exec.Command("/bin/sh", "-c", script)
	return cmd.Run()
}

func addFilter(log log.Logger, nic string, direction string) error {
	// add the pfc ingress filter to the nic if it's not already there
	script := fmt.Sprintf("tc filter show dev %[1]s ingress | grep pfc_%[2]s_tc || tc filter add dev %[1]s ingress bpf direct-action object-file /opt/acnodal/bin/pfc_%[2]s_tc.o sec .text", nic, direction)
	log.Log("script", script)
	cmd := exec.Command("/bin/sh", "-c", script)
	return cmd.Run()
}

func configurePFC(log log.Logger, nic string, qid int, flags int) error {
	// configure the PFC only if it hasn't been already
	script := fmt.Sprintf("/opt/acnodal/bin/cli_cfg get %[1]s | grep %[1]s || /opt/acnodal/bin/cli_cfg set %[1]s %[2]d 0 %[3]d \"%[1]s rx\"", nic, qid, flags)
	log.Log("script", script)
	cmd := exec.Command("/bin/sh", "-c", script)
	return cmd.Run()
}
