// Copyright 2020 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package netbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
)

// Netbox represents a connection to a
// [Netbox](https://netbox.readthedocs.io/) IPAM system.
type Netbox struct {
	http http.Client
	// The base URL of the Netbox system.
	base string
	// The Netbox tenant slug.
	tenant string
	// The Netbox user token that PureLB uses to authenticate.
	token string
}

type address struct {
	ID      int
	Address string
}
type addressQueryResponse struct {
	Count   int
	Results []address
}

// NewNetbox configures a new connection to a Netbox system.
func NewNetbox(base string, tenant string, token string) *Netbox {
	return &Netbox{http: http.Client{}, base: base, tenant: tenant, token: token}
}

func (n *Netbox) newRequest(verb string, url string) (*http.Request, error) {
	req, err := http.NewRequest(verb, n.base+url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("accept", "application/json")
	req.Header.Add("Authorization", "Token "+n.token)
	return req, nil
}

func (n *Netbox) newGetRequest(url string) (*http.Request, error) {
	return n.newRequest(http.MethodGet, url)
}

func (n *Netbox) newPatchRequest(url string, body []byte) (*http.Request, error) {
	req, err := n.newRequest(http.MethodPatch, url)
	if err != nil {
		return nil, err
	}
	req.Body = ioutil.NopCloser(bytes.NewReader(body))
	return req, nil
}

// fetchAddrs finds out if Netbox has any available addresses. An
// address is available if it belongs to our tenant and its status
// matches the status parameter.
func (n *Netbox) fetchAddrs(tenant string, status string) ([]address, error) {
	req, err := n.newGetRequest("api/ipam/ip-addresses/")
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = url.Values{"tenant": []string{tenant}, "status": []string{status}}.Encode()
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// FIXME: Netbox is not good about HTTP response codes.  If our
	// authn fails then Netbox 302's to a login page even though we
	// specified that we want a JSON response. We need to treat 302 as
	// an error.

	var body addressQueryResponse
	err = json.NewDecoder(resp.Body).Decode(&body)
	if err != nil {
		return nil, err
	}
	if body.Count < 1 {
		return nil, fmt.Errorf("No addresses available")
	}

	return body.Results, nil
}

func (n *Netbox) allocateAddr(addr address) error {
	// mark the address as "in use" by sending an HTTP PATCH request to
	// set the Netbox address status to "active"
	url := fmt.Sprintf("api/ipam/ip-addresses/%d/", addr.ID)
	req, err := n.newPatchRequest(url, []byte("{\"status\": \"active\"}"))
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

// Fetch fetches an address from Netbox. If the fetch is successful
// then error will be nil and the returned string will describe an
// address.
func (n *Netbox) Fetch() (string, error) {
	var (
		ipStatus string = "reserved"
	)

	// fetch list of addresses
	addrs, err := n.fetchAddrs(n.tenant, ipStatus)
	if err != nil {
		return "", err
	}

	first := addrs[0]
	err = n.allocateAddr(addrs[0])

	return first.Address, err
}
