package acnodal

import (
	"fmt"
	"os"

	"github.com/go-resty/resty/v2"
)

type EGW struct {
	http       resty.Client
	base       string
	auth_token string
}

type EGWGroup struct {
	ID string
}

type EGWService struct {
	ID        string `json:"id,omitempty"`
	Name      string
	Address   string
	Self      string `json:"id,omitempty"`
	Endpoints string `json:"id,omitempty"`
}

type EGWServiceCreate struct {
	Group   EGWGroup
	Service EGWService
}
type EGWServiceResponse struct {
	Group   EGWGroup
	Service EGWService
}

	Service EGWService
}

func New(base string, auth_token string) (*EGW, error) {
	var is_set bool
	if base == "" {
		base, is_set = os.LookupEnv("NETBOX_BASE_URL")
		if !is_set {
			fmt.Println("NETBOX_BASE_URL not set, can't connect to Netbox")
			return nil, fmt.Errorf("NETBOX_BASE_URL not set, can't connect to Netbox")
		}
	}
	if auth_token == "" {
		auth_token, is_set = os.LookupEnv("NETBOX_USER_TOKEN")
		if !is_set {
			fmt.Println("NETBOX_USER_TOKEN not set, can't connect to Netbox")
			return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
		}
	}

	r := resty.New().
		SetHostURL(base).
		SetHeaders(map[string]string{
			"Content-Type": "application/json",
			"accept":       "application/json",
		})
	return &EGW{http: *r, base: base, auth_token: auth_token}, nil
}

func (n *EGW) AnnounceService(groupId string, name string, address string) (service EGWService, err error) {
	response, err := n.http.R().
		SetBody(EGWServiceCreate{
			Group:   EGWGroup{ID: groupId},
			Service: EGWService{Name: name, Address: address}}).
		SetResult(EGWServiceResponse{}).
		Post("/api/egw/services/")
	if err != nil {
		return EGWService{}, err
	}
	if response.IsError() {
		return EGWService{}, fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return srv.Service, nil
}

func (n *EGW) AnnounceEndpoint(svcname string, svcid string, svcaddress string, endpoint string) (serviceId string, err error) {
	response, err := n.http.R().
		SetResult(EGWServiceResponse{}).
		Post("/api/egw/endpoint/")
	if err != nil {
		return "", err
	}
	if response.IsError() {
		return "", fmt.Errorf("response code %d status %s", response.StatusCode(), response.Status())
	}

	srv := response.Result().(*EGWServiceResponse)
	return srv.Service.ID, nil
}
