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
	"fmt"
	"net/url"
	"strings"

	"github.com/go-resty/resty/v2"
	v1 "k8s.io/api/core/v1"
)

// EGW represents one connection to an Acnodal Enterprise Gateway.
type EGW interface {
	GetGroup() (EGWGroupResponse, error)
	AnnounceService(url string, name string, ports []v1.ServicePort) (EGWServiceResponse, error)
	FetchService(url string) (EGWServiceResponse, error)
	WithdrawService(svcUrl string) error
	AnnounceEndpoint(url string, address string, port v1.EndpointPort, nodeAddress string) (*EGWEndpointResponse, error)
}

// egw represents one connection to an Acnodal Enterprise Gateway.
type egw struct {
	http      resty.Client
	groupURL  string
	authToken string
}

// Links holds a map of URL strings.
type Links map[string]string

// ObjectMeta is a shadow of the k8s ObjectMeta struct.
type ObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// EGWGroup is the on-the-wire representation of one Service Group.
type EGWGroup struct {
	ObjectMeta ObjectMeta `json:"metadata"`
}

// EGWServiceSpec is the on-the-wire representation of one
// LoadBalancer Service Spec (i.e., the part that defines what the LB
// should look like).
type EGWServiceSpec struct {
	Address string           `json:"public-address,omitempty"`
	Ports   []v1.ServicePort `json:"public-ports"`
	GUEKey  uint32           `json:"gue-key"`
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

// EGWEndpoint is the on-the-wire representation of one LoadBalancer
// endpoint.
type EGWEndpoint struct {
	Address     string
	Port        v1.EndpointPort
	NodeAddress string `json:"node-address"`
}

// EGWGroupResponse is the body of the HTTP response to a request to
// show a service group.
type EGWGroupResponse struct {
	Links Links    `json:"link"`
	Group EGWGroup `json:"group"`
}

// EGWServiceCreate is the body of the HTTP request to create a load
// balancer service.
type EGWServiceCreate struct {
	Service EGWService `json:"service"`
}

// EGWServiceResponse is the body of the HTTP response to a request to
// show a load balancer.
type EGWServiceResponse struct {
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
	Message string     `json:"message,omitempty"`
	Service EGWService `json:"service,omitempty"`
}

// New initializes a new EGW instance. If error is non-nil then the
// instance shouldn't be used.
func NewEGW(groupURL string) (EGW, error) {
	// Use the hostname from the service group, but reset the path.  EGW
	// and Netbox each have their own API URL schemes so we only need
	// the protocol, host, port, credentials, etc.
	baseURL, err := url.Parse(groupURL)
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
		SetRedirectPolicy(resty.FlexibleRedirectPolicy(2))

	// Initialize the EGW instance
	return &egw{http: *r, groupURL: groupURL}, nil
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
		return EGWServiceResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return *srv, nil
}

func (n *egw) FetchService(url string) (EGWServiceResponse, error) {
	response, err := n.http.R().
		SetResult(EGWServiceResponse{}).
		Get(url)
	if err != nil {
		return EGWServiceResponse{}, err
	}
	if response.IsError() {
		return EGWServiceResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return *srv, nil
}

// AnnounceEndpoint announces an endpoint to the EGW.
func (n *egw) AnnounceEndpoint(url string, address string, ePort v1.EndpointPort, nodeAddress string) (*EGWEndpointResponse, error) {
	response, err := n.http.R().
		SetBody(EGWEndpointCreate{Endpoint: EGWEndpoint{Address: address, Port: ePort, NodeAddress: nodeAddress}}).
		SetError(EGWEndpointResponse{}).
		SetResult(EGWEndpointResponse{}).
		Post(url)
	if err != nil {
		return &EGWEndpointResponse{}, err
	}
	if response.IsError() {
		// if the response indicates that this endpoint is already
		// announced then it's not an error
		if response.StatusCode() == 409 {
			if strings.Contains(response.Error().(*EGWEndpointResponse).Message, "duplicate endpoint") {
				return response.Error().(*EGWEndpointResponse), nil
			}
		}

		return &EGWEndpointResponse{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}
	return response.Result().(*EGWEndpointResponse), nil
}

// WithdrawService tells the EGW that this service should be deleted.
func (n *egw) WithdrawService(url string) error {
	response, err := n.http.R().Delete(url)
	if err != nil {
		return err
	}
	if response.IsError() {
		return fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}
	return nil
}
