// Copyright 2020 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:generate mockgen -destination internal/acnodal/mock_egw.go -package purelb.io/internal/acnodal

package acnodal

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-resty/resty/v2"
	v1 "k8s.io/api/core/v1"

	purelbv1 "purelb.io/pkg/apis/v1"
)

const (
	locationHeader = "Location"
)

// EGW represents one connection to an Acnodal Enterprise Gateway.
type EGW interface {
	GetAccount() (EGWAccountResponse, error)
	GetGroup() (EGWGroupResponse, error)
	AnnounceService(url string, name string, ports []v1.ServicePort) (EGWServiceResponse, error)
	FetchService(url string) (EGWServiceResponse, error)
	Delete(svcUrl string) error
	AnnounceEndpoint(url string, nsName string, address string, port v1.EndpointPort, nodeAddress string) (*EGWEndpointResponse, error)
	AddCluster(createClusterURL string, nsName string) (EGWClusterResponse, error)
	FetchCluster(clusterURL string) (EGWClusterResponse, error)
}

// egw represents one connection to an Acnodal Enterprise Gateway.
type egw struct {
	http      resty.Client
	groupURL  string
	authToken string
	myCluster string
}

// Links holds a map of URL strings.
type Links map[string]string

// ObjectMeta is a shadow of the k8s ObjectMeta struct.
type ObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// EGWAccount is the on-the-wire representation of one EGW Account.
type EGWAccount struct {
	ObjectMeta ObjectMeta     `json:"metadata"`
	Spec       EGWAccountSpec `json:"spec"`
}

// EGWAccountSpec is the on-the-wire representation of one Account
// Spec (i.e., the part that defines what the Account should look
// like).
type EGWAccountSpec struct {
	GroupID uint16 `json:"group-id"`
}

// EGWGroup is the on-the-wire representation of one Service Group.
type EGWGroup struct {
	ObjectMeta ObjectMeta `json:"metadata"`
}

// EGWServiceSpec is the on-the-wire representation of one
// LoadBalancer Service Spec (i.e., the part that defines what the LB
// should look like).
type EGWServiceSpec struct {
	Address   string           `json:"public-address,omitempty"`
	Ports     []v1.ServicePort `json:"public-ports"`
	ServiceID uint16           `json:"service-id"`

	// TunnelKey authenticates the client with the EGW. It must be a
	// base64-encoded 128-bit value.
	TunnelKey string `json:"tunnel-key,omitempty"`
}

// EGWTunnelEndpoint is an Endpoint on the EGW.
type EGWTunnelEndpoint struct {
	// Address is the IP address for this endpoint.
	Address string `json:"egw-address"`

	// Port is the port on which this endpoint listens.
	Port v1.EndpointPort `json:"egw-port"`

	// TunnelID distinguishes the traffic using this tunnel from the
	// traffic using other tunnels that end on the same host.
	TunnelID uint32 `json:"tunnel-id"`
}

type EGWServiceStatus struct {
	// GUETunnelEndpoints is a map from client node addresses to public
	// GUE tunnel endpoints on the EGW. The map key is a client node
	// address and must be one of the node addresses in the Spec
	// Endpoints slice. The value is a GUETunnelEndpoint that describes
	// the public IP and port to which the client can send tunnel ping
	// packets.
	EGWTunnelEndpoints map[string]EGWTunnelEndpoint `json:"gue-tunnel-endpoints"`
}

// EGWService is the on-the-wire representation of one LoadBalancer
// Service.
type EGWService struct {
	ObjectMeta ObjectMeta       `json:"metadata"`
	Spec       EGWServiceSpec   `json:"spec"`
	Status     EGWServiceStatus `json:"status,omitempty"`
}

// EGWEndpointSpec is the on-the-wire representation of one EGW
// endpoint specification.
type EGWEndpointSpec struct {
	Address     string `json:"address"`
	Port        v1.EndpointPort
	NodeAddress string `json:"node-address"`
	// Cluster is the name of the cluster to which this rep belongs.
	Cluster string `json:"cluster"`
}

// EGWEndpoint is the on-the-wire representation of one LoadBalancer
// endpoint.
type EGWEndpoint struct {
	ObjectMeta ObjectMeta      `json:"metadata"`
	Spec       EGWEndpointSpec `json:"spec"`
}

// EGWAccountResponse is the body of the HTTP response to a request to
// show an account.
type EGWAccountResponse struct {
	Links   Links      `json:"link"`
	Account EGWAccount `json:"account"`
}

// EGWGroupResponse is the body of the HTTP response to a request to
// show a service group.
type EGWGroupResponse struct {
	Message string   `json:"message,omitempty"`
	Links   Links    `json:"link"`
	Group   EGWGroup `json:"group"`
}

// EGWServiceCreate is the body of the HTTP request to create a load
// balancer service.
type EGWServiceCreate struct {
	Service EGWService `json:"service"`
}

// EGWServiceResponse is the body of the HTTP response to a request to
// show a load balancer.
type EGWServiceResponse struct {
	Message string     `json:"message,omitempty"`
	Links   Links      `json:"link"`
	Service EGWService `json:"service"`
}

// EGWEndpointCreate is the body of the HTTP request to create a load
// balancer endpoint.
type EGWEndpointCreate struct {
	Endpoint EGWEndpoint
}

// EGWEndpointResponse is the body of the HTTP response to a request to
// show a load balancer endpoint.
type EGWEndpointResponse struct {
	Message  string      `json:"message,omitempty"`
	Links    Links       `json:"link"`
	Endpoint EGWEndpoint `json:"endpoint,omitempty"`
}

// EGWClusterCreate is the body of the HTTP request to create a load
// balancer cluster.
type EGWClusterCreate struct {
	ClusterID string `json:"cluster-id"`
}

// EGWClusterResponse is the body of the HTTP response to a request to
// show a load balancer cluster.
type EGWClusterResponse struct {
	Message string `json:"message,omitempty"`
	Links   Links  `json:"link"`
}

// New initializes a new EGW instance. If error is non-nil then the
// instance shouldn't be used.
func NewEGW(myCluster string, group purelbv1.ServiceGroupEGWSpec) (EGW, error) {
	// Use the hostname from the service group, but reset the path.  EGW
	// and Netbox each have their own API URL schemes so we only need
	// the protocol, host, port, credentials, etc.
	baseURL, err := url.Parse(group.URL)
	if err != nil {
		return nil, err
	}
	baseURL.Path = "/"

	// Set up a REST client to talk to the EGW
	r := resty.New().
		SetHostURL(baseURL.String()).
		SetHeaders(map[string]string{
			"Content-Type": "application/json",
			"accept":       "application/json",
		}).
		SetBasicAuth(group.WSUsername, group.WSPassword).
		SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true}). // FIXME: figure out how to *not* disable cert checks
		SetRedirectPolicy(resty.FlexibleRedirectPolicy(2))

	// Initialize the EGW instance
	return &egw{http: *r, groupURL: group.URL, myCluster: myCluster}, nil
}

// GetAccount requests an account from the EGW.
func (n *egw) GetAccount() (EGWAccountResponse, error) {
	group, err := n.GetGroup()
	if err != nil {
		return EGWAccountResponse{}, err
	}

	response, err := n.http.R().
		SetResult(EGWAccountResponse{}).
		Get(group.Links["account"])
	if err != nil {
		return EGWAccountResponse{}, err
	}
	if response.IsError() {
		return EGWAccountResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWAccountResponse)
	return *srv, nil
}

// GetGroup requests a service group from the EGW.
func (n *egw) GetGroup() (EGWGroupResponse, error) {
	response, err := n.http.R().
		SetResult(EGWGroupResponse{}).
		Get(n.groupURL)
	if err != nil {
		return EGWGroupResponse{}, err
	}
	if response.IsError() {
		return EGWGroupResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWGroupResponse)
	return *srv, nil
}

// AnnounceService announces a service to the EGW. url is the service
// creation URL which is a child of the service group to which this
// service will belong. name is the service name.  address is a string
// containing an IP address. ports is a slice of v1.ServicePorts.
func (n *egw) AnnounceService(url string, name string, sPorts []v1.ServicePort) (EGWServiceResponse, error) {
	// send the request
	response, err := n.http.R().
		SetBody(EGWServiceCreate{
			Service: EGWService{ObjectMeta: ObjectMeta{Name: name}, Spec: EGWServiceSpec{Ports: sPorts}}}).
		SetResult(EGWServiceResponse{}).
		Post(url)
	if err != nil {
		return EGWServiceResponse{}, err
	}
	if response.IsError() {
		// if the response indicates that this service is already
		// announced then it's not necessarily an error. Try to fetch the
		// service and return that.
		if response.StatusCode() == http.StatusConflict {
			return n.FetchService(response.Header().Get(locationHeader))
		}

		return EGWServiceResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return *srv, nil
}

// FetchService fetches the service at "url" from the EPIC.
func (n *egw) FetchService(url string) (EGWServiceResponse, error) {
	response, err := n.http.R().
		SetResult(EGWServiceResponse{}).
		Get(url)
	if err != nil {
		return EGWServiceResponse{}, err
	}
	if response.IsError() {
		return EGWServiceResponse{}, fmt.Errorf("%s response code %d status %s", url, response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return *srv, nil
}

// AddCluster adds a cluster to an EPIC
// LoadBalancer. "createClusterURL" is the value of the
// "create-cluster" key in the service's "link" object. name is the
// cluster name.
func (n *egw) AddCluster(createClusterURL string, svcName string) (EGWClusterResponse, error) {
	// send the request
	response, err := n.http.R().
		SetBody(EGWClusterCreate{ClusterID: clusterName(n.myCluster, svcName)}).
		SetResult(EGWClusterResponse{}).
		Post(createClusterURL)
	if err != nil {
		return EGWClusterResponse{}, err
	}
	if response.IsError() {
		// if the response indicates that this cluster is already
		// announced then it's not necessarily an error. Try to fetch the
		// cluster and return that.
		if response.StatusCode() == http.StatusConflict {
			return n.FetchCluster(response.Header().Get(locationHeader))
		}

		return EGWClusterResponse{}, fmt.Errorf("%s response code %d status \"%s\"", createClusterURL, response.StatusCode(), response.Status())
	}

	cluster := response.Result().(*EGWClusterResponse)
	return *cluster, nil
}

// FetchCluster fetches the cluster at "clusterURL" from the EPIC.
func (n *egw) FetchCluster(clusterURL string) (EGWClusterResponse, error) {
	response, err := n.http.R().
		SetResult(EGWClusterResponse{}).
		Get(clusterURL)
	if err != nil {
		return EGWClusterResponse{}, err
	}
	if response.IsError() {
		return EGWClusterResponse{}, fmt.Errorf("%s response code %d status \"%s\"", clusterURL, response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWClusterResponse)
	return *srv, nil
}

// AnnounceEndpoint announces an endpoint to the EGW.
func (n *egw) AnnounceEndpoint(url string, svcName string, address string, ePort v1.EndpointPort, nodeAddress string) (*EGWEndpointResponse, error) {

	response, err := n.http.R().
		SetBody(EGWEndpointCreate{
			Endpoint: EGWEndpoint{Spec: EGWEndpointSpec{Cluster: clusterName(n.myCluster, svcName), Address: address, Port: ePort, NodeAddress: nodeAddress}}}).
		SetError(EGWEndpointResponse{}).
		SetResult(EGWEndpointResponse{}).
		Post(url)
	if err != nil {
		return &EGWEndpointResponse{}, err
	}
	if response.IsError() {
		// if the response indicates that this endpoint is already
		// announced then it's not an error
		if response.StatusCode() == http.StatusConflict {
			if strings.Contains(response.Error().(*EGWEndpointResponse).Message, "duplicate endpoint") {
				return response.Error().(*EGWEndpointResponse), nil
			}
		}

		return &EGWEndpointResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}
	return response.Result().(*EGWEndpointResponse), nil
}

// Delete tells the EGW that this object should be deleted.
func (n *egw) Delete(url string) error {
	response, err := n.http.R().Delete(url)
	if err != nil {
		return err
	}
	if response.IsError() {
		return fmt.Errorf("response code %d status \"%s\"", response.StatusCode(), response.Status())
	}
	return nil
}

// clusterName returns a string that meets k8s' requirements for label
// values.
func clusterName(clusterID string, svcNSName string) string {
	return rfc1123Cleaner.Replace(clusterID + "-" + svcNSName)
}
