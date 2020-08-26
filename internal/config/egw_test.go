package config

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	GroupName       = "acnodal-test"
	ServiceName     = "test-service"
	ServiceAddress  = "192.168.1.27"
	EndpointName    = "test-endpoint"
	EndpointAddress = "10.42.27.42"
	EndpointPort    = 80
	GroupURL        = "/api/egw/groups/b321256d-31b7-4209-bd76-28dec3c77c25" // FIXME: use c.ips.Pool(name) but it's safer to hard-code for now
)

func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Short() {
		fmt.Println("Skipping egw tests because short testing was requested.")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func NewEGW(t *testing.T) *EGW {
	e, err := New("", "")
	if err != nil {
		t.Fatal("initializing EGW", err)
	}
	return e
}

func GetGroup(t *testing.T, e *EGW, url string) EGWGroup {
	g, err := e.GetGroup(url)
	if err != nil {
		t.Fatal("getting group", err)
	}
	return g
}

func TestGetGroup(t *testing.T) {
	e := NewEGW(t)
	g := GetGroup(t, e, GroupURL)
	if g.Name != GroupName {
		t.Fatal("group name mismatch", g.Name, GroupName)
	}
}

func TestAnnounceService(t *testing.T) {
	e := NewEGW(t)
	g := GetGroup(t, e, GroupURL)
	svc, err := e.AnnounceService(g.Links["create-service"], ServiceName, ServiceAddress)
	if err != nil {
		t.Fatal("announcing service", err)
	}
	assert.Equal(t, svc.Links["group"], GroupURL, "group url mismatch")
}

func TestAnnounceEndpoint(t *testing.T) {
	e := NewEGW(t)
	g := GetGroup(t, e, GroupURL)
	s, _ := e.AnnounceService(g.Links["create-service"], ServiceName, ServiceAddress)
	err := e.AnnounceEndpoint(s.Links["create-endpoint"], EndpointAddress, EndpointPort)
	if err != nil {
		t.Errorf("got error %+v", err)
	}
}
