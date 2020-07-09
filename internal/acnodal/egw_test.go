package acnodal

import (
	"flag"
	"fmt"
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Short() {
		fmt.Println("Skipping egw tests because short testing was requested.")
		os.Exit(0)
	}
	if _, env_set := os.LookupEnv("NETBOX_BASE_URL"); !env_set {
		log.Fatalf("NETBOX_BASE_URL not set, can't connect to Netbox")
	}
	if _, env_set := os.LookupEnv("NETBOX_USER_TOKEN"); !env_set {
		log.Fatalf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
	}
	os.Exit(m.Run())
}

func TestAnnounceService(t *testing.T) {
	e, err := New("", "")
	id, err := e.AnnounceService("test-groupid", "test-service", "192.168.1.27")
	if err != nil {
		t.Errorf("got error %+v", err)
	} else if id != "42" {
		t.Errorf("got invalid service id %s", id)
	}
}

func TestAnnounceEndpoint(t *testing.T) {
	e, err := New("", "")
	id, err := e.AnnounceEndpoint("test-endpoint", "42", "192.168.1.27", "10.42.27.42")
	if err != nil {
		t.Errorf("got error %+v", err)
	} else if id != "42" {
		t.Errorf("got invalid endpoint id %s", id)
	}
}
