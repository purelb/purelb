package config

import (
	"fmt"
	"os"

	"github.com/go-resty/resty/v2"
)

// FIXME: package these in the EGW so we can reference them here
type Links map[string]string

type EGW struct {
	http       resty.Client
	base       string
	auth_token string
}

type EGWGroup struct {
	Name      string
	Links     Links  `json:"link"`
	Created   string `json:"created,omitempty"`
	Updated   string `json:"updated,omitempty"`
}

type EGWService struct {
	Name      string
	Address   string
	Endpoints string `json:"id,omitempty"`
	Links     Links  `json:"link"`
	Created   string `json:"created,omitempty"`
	Updated   string `json:"updated,omitempty"`
}

type EGWEndpoint struct {
	Address string
	Port    int
	Links   Links  `json:"link"`
	Created string `json:"created,omitempty"`
	Updated string `json:"updated,omitempty"`
}

type EGWServiceCreate struct {
	Service EGWService
}
type EGWServiceResponse struct {
	Service EGWService
}

type EGWEndpointCreate struct {
	Endpoint EGWEndpoint
}
type EGWEndpointResponse struct {
	Service EGWService
}

func New(base string, auth_token string) (*EGW, error) {
	var is_set bool
	if base == "" {
		base, is_set = os.LookupEnv("NETBOX_BASE_URL")
		if !is_set {
			return nil, fmt.Errorf("NETBOX_BASE_URL not set, can't connect to Netbox")
		}
	}
	if auth_token == "" {
		auth_token, is_set = os.LookupEnv("NETBOX_USER_TOKEN")
		if !is_set {
			return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
		}
	}

	r := resty.New().
		SetHostURL(base).
		SetHeaders(map[string]string{
			"Content-Type": "application/json",
			"accept":       "application/json",
		}).
		SetRedirectPolicy(resty.FlexibleRedirectPolicy(2))
	return &EGW{http: *r, base: base, auth_token: auth_token}, nil
}

func (n *EGW) GetGroup(url string) (EGWGroup, error) {
	response, err := n.http.R().
		SetResult(EGWGroup{}).
		Get(url)
	if err != nil {
		return EGWGroup{}, err
	}
	if response.IsError() {
		return EGWGroup{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWGroup)
	return *srv, nil
}

func (n *EGW) AnnounceService(url string, name string, address string) (EGWService, error) {
	response, err := n.http.R().
		SetBody(EGWServiceCreate{
			Service: EGWService{Name: name, Address: address}}).
		SetResult(EGWService{}).
		Post(url)
	if err != nil {
		return EGWService{}, err
	}
	if response.IsError() {
		return EGWService{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWService)
	return *srv, nil
}

func (n *EGW) AnnounceEndpoint(url string, endpoint string, port int) error {
	response, err := n.http.R().
		SetBody(EGWEndpointCreate{
			Endpoint: EGWEndpoint{Address: endpoint, Port: port}}).
		SetResult(EGWServiceResponse{}).
		Post(url)
	if err != nil {
		return err
	}
	if response.IsError() {
		return fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}
	return nil
}
