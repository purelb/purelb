package netbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
)

type Netbox struct {
	http       http.Client
	base       string
	auth_token string
}

type Address struct {
	ID      int
	Address string
}
type AddressQueryResponse struct {
	Count   int
	Results []Address
}

func New(base string, auth_token string) *Netbox {
	return &Netbox{http: http.Client{}, base: base, auth_token: auth_token}
}

func (n *Netbox) NewRequest(verb string, url string) (*http.Request, error) {
	req, err := http.NewRequest(verb, n.base+url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("accept", "application/json")
	req.Header.Add("Authorization", "Token "+n.auth_token)
	return req, nil
}

func (n *Netbox) NewGetRequest(url string) (*http.Request, error) {
	return n.NewRequest(http.MethodGet, url)
}

func (n *Netbox) NewPatchRequest(url string, body []byte) (*http.Request, error) {
	req, err := n.NewRequest(http.MethodPatch, url)
	if err != nil {
		return nil, err
	}
	req.Body = ioutil.NopCloser(bytes.NewReader(body))
	return req, nil
}

func (n *Netbox) fetchAddrs(tenant string, status string) ([]Address, error) {
	req, err := n.NewGetRequest("/api/ipam/ip-addresses/")
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = url.Values{"tenant": []string{tenant}, "status": []string{status}}.Encode()
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body AddressQueryResponse
	err = json.NewDecoder(resp.Body).Decode(&body)
	if err != nil {
		return nil, err
	}
	if body.Count < 1 {
		fmt.Println("zero addresses available")
		return nil, fmt.Errorf("No addresses available")
	}

	return body.Results, nil
}

func (n *Netbox) allocateAddr(addr Address) error {
	url := fmt.Sprintf("/api/ipam/ip-addresses/%d/", addr.ID)
	req, err := n.NewPatchRequest(url, []byte("{\"status\": \"active\"}"))
	if err != nil {
		return err
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	return err
}

func (n *Netbox) Fetch() (string, error) {
	var (
		tenant    string = "ipam-metallb-customer-exp"
		ip_status string = "reserved"
	)

	// fetch list of addresses
	addrs, err := n.fetchAddrs(tenant, ip_status)
	if err != nil {
		return "", err
	}

	first := addrs[0]
	fmt.Println("allocating ", first.Address)
	err = n.allocateAddr(addrs[0])

	return first.Address, nil
}
