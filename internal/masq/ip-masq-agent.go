/*
Copyright 2017 The Kubernetes Authors.
Copyright 2022 Acnodal Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package masq

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	utiljson "encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilexec "k8s.io/utils/exec"

	"github.com/golang/glog"

	"purelb.io/internal/masq/testing/fakefs"
)

const (
	IP_FAMILY_V4 = string(v1.IPv4Protocol)
	IP_FAMILY_V6 = string(v1.IPv6Protocol)

	CHAIN_PREFIX string = "PURELB-"

	linkLocalCIDR = "169.254.0.0/16"
	// RFC 4291
	linkLocalCIDRIPv6 = "fe80::/10"
	// path to a yaml or json file
	configPath = "/etc/config/ip-masq-agent"
)

var (
	masqChainFlag                     = flag.String("masq-chain", "IP-MASQ-AGENT", `Name of nat chain for iptables masquerade rules.`)
	noMasqueradeAllReservedRangesFlag = flag.Bool("nomasq-all-reserved-ranges", false, "Whether to disable masquerade for all IPv4 ranges reserved by RFCs.")
	enableIPv6                        = flag.Bool("enable-ipv6", true, "Whether to enable IPv6.")
)

// MasqConfig object
type MasqConfig struct {
	NonMasqueradeCIDRs []string `json:"nonMasqueradeCIDRs"`
	MasqLinkLocal      bool     `json:"masqLinkLocal"`
	MasqLinkLocalIPv6  bool     `json:"masqLinkLocalIPv6"`
	ResyncInterval     Duration `json:"resyncInterval"`
}

// Duration - Go's JSON unmarshaler can't handle time.ParseDuration syntax when unmarshaling into time.Duration, so we do it here
type Duration time.Duration

// UnmarshalJSON ...
func (d *Duration) UnmarshalJSON(json []byte) error {
	if json[0] == '"' {
		s := string(json[1 : len(json)-1])
		t, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(t)
		return nil
	}
	s := string(json)
	return fmt.Errorf("expected string value for unmarshal to field of type Duration, got %q", s)
}

// NewMasqConfig returns a MasqConfig with default values
func NewMasqConfig(masqAllReservedRanges bool) *MasqConfig {
	// RFC 1918 defines the private ip address space as 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16
	nonMasq := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

	if masqAllReservedRanges {
		nonMasq = append(nonMasq,
			"100.64.0.0/10",   // RFC 6598
			"192.0.0.0/24",    // RFC 6890
			"192.0.2.0/24",    // RFC 5737
			"192.88.99.0/24",  // RFC 7526
			"198.18.0.0/15",   // RFC 6815
			"198.51.100.0/24", // RFC 5737
			"203.0.113.0/24",  // RFC 5737
			"240.0.0.0/4")     // RFC 5735, Former Class E range obsoleted by RFC 3232
	}

	return &MasqConfig{
		NonMasqueradeCIDRs: nonMasq,
		MasqLinkLocal:      false,
		MasqLinkLocalIPv6:  false,
		ResyncInterval:     Duration(60 * time.Second),
	}
}

// MasqDaemon object
type MasqDaemon struct {
	config    *MasqConfig
	iptables  utiliptables.Interface
	ip6tables utiliptables.Interface
}

// NewMasqDaemon returns a MasqDaemon with default values, including an initialized utiliptables.Interface
func NewMasqDaemon(c *MasqConfig) *MasqDaemon {
	execer := utilexec.New()
	protocolv4 := utiliptables.ProtocolIPv4
	protocolv6 := utiliptables.ProtocolIPv6
	iptables := utiliptables.New(execer, protocolv4)
	ip6tables := utiliptables.New(execer, protocolv6)
	return &MasqDaemon{
		config:    c,
		iptables:  iptables,
		ip6tables: ip6tables,
	}
}

func (m *MasqDaemon) UpdateConfig(c MasqConfig) error {
	// validate configuration
	if err := c.validate(); err != nil {
		return err
	}

	// apply new config
	m.config = &c

	return nil
}

func (m *MasqDaemon) osSyncConfig() error {
	// the fakefs.FileSystem interface allows us to mock the fs from tests
	// fakefs.DefaultFS implements fakefs.FileSystem using os.Stat and io/ioutil.ReadFile
	var fs fakefs.FileSystem = fakefs.DefaultFS{}
	return m.syncConfig(fs)
}

// Syncs the config to the file at ConfigPath, or uses defaults if the file could not be found
// Error if the file is found but cannot be parsed.
func (m *MasqDaemon) syncConfig(fs fakefs.FileSystem) error {
	var err error
	c := NewMasqConfig(*noMasqueradeAllReservedRangesFlag)
	defer func() {
		if err == nil {
			json, _ := utiljson.Marshal(c)
			glog.V(2).Infof("using config: %s", string(json))
		}
	}()

	// check if file exists
	if _, err = fs.Stat(configPath); os.IsNotExist(err) {
		// file does not exist, use defaults
		m.config.NonMasqueradeCIDRs = c.NonMasqueradeCIDRs
		m.config.MasqLinkLocal = c.MasqLinkLocal
		m.config.MasqLinkLocalIPv6 = c.MasqLinkLocalIPv6
		m.config.ResyncInterval = c.ResyncInterval
		glog.V(2).Infof("no config file found at %q, using default values", configPath)
		return nil
	}
	glog.V(2).Infof("config file found at %q", configPath)

	// file exists, read and parse file
	yaml, err := fs.ReadFile(configPath)
	if err != nil {
		return err
	}

	json, err := utilyaml.ToJSON(yaml)
	if err != nil {
		return err
	}

	// Only overwrites fields provided in JSON
	if err = utiljson.Unmarshal(json, c); err != nil {
		return err
	}

	// apply new config
	return m.UpdateConfig(*c)
}

func (c *MasqConfig) validate() error {
	// limit to 64 CIDRs (excluding link-local) to protect against really bad mistakes
	n := len(c.NonMasqueradeCIDRs)
	if n > 64 {
		return fmt.Errorf("the daemon can only accept up to 64 CIDRs (excluding link-local), but got %d CIDRs (excluding link local)", n)
	}
	// check CIDRs are valid
	for _, cidr := range c.NonMasqueradeCIDRs {
		if err := validateCIDR(cidr); err != nil {
			return err
		}
		// can't configure ipv6 cidr if ipv6 is not enabled
		if !*enableIPv6 && isIPv6CIDR(cidr) {
			return fmt.Errorf("ipv6 is not enabled, but ipv6 cidr %s provided. Enable ipv6 using --enable-ipv6 agent flag", cidr)
		}
	}
	return nil
}

const cidrParseErrFmt = "CIDR %q could not be parsed, %v"
const cidrAlignErrFmt = "CIDR %q is not aligned to a CIDR block, ip: %q network: %q"

func validateCIDR(cidr string) error {
	// parse test
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf(cidrParseErrFmt, cidr, err)
	}
	// alignment test
	if !ip.Equal(ipnet.IP) {
		return fmt.Errorf(cidrAlignErrFmt, cidr, ip, ipnet.String())
	}
	return nil
}

func (m *MasqDaemon) SyncChains(nsName string, ingress []v1.LoadBalancerIngress) error {
	chainV4 := utiliptables.Chain(ChainNameV4(nsName))
	chainV6 := utiliptables.Chain(ChainNameV6(nsName))

	// make sure our custom chains for non-masquerade exist
	m.iptables.EnsureChain(utiliptables.TableNAT, chainV4)
	_, err := m.ip6tables.EnsureChain(utiliptables.TableNAT, chainV6)
	if err != nil {
		return err
	}

	// build up lines to pass to iptables-restore
	linesV4 := bytes.NewBuffer(nil)
	writeLine(linesV4, "*nat")
	writeLine(linesV4, utiliptables.MakeChainLine(chainV4)) // effectively flushes chain atomically with rule restore
	linesV6 := bytes.NewBuffer(nil)
	writeLine(linesV6, "*nat")
	writeLine(linesV6, utiliptables.MakeChainLine(chainV6))

	// link-local CIDR is always non-masquerade
	if !m.config.MasqLinkLocal {
		writeNonMasqRule(linesV4, linkLocalCIDR, chainV4)
	}
	if !m.config.MasqLinkLocalIPv6 {
		writeNonMasqRule(linesV6, linkLocalCIDRIPv6, chainV6)
	}

	// non-masquerade for user-provided CIDRs
	for _, cidr := range m.config.NonMasqueradeCIDRs {
		if !isIPv6CIDR(cidr) {
			writeNonMasqRule(linesV4, cidr, chainV4)
		} else {
			writeNonMasqRule(linesV6, cidr, chainV6)
		}
	}

	// masquerade all other traffic that is not bound for a --dst-type
	// LOCAL destination
	for _, addr := range ingress {
		ip := addr.IP
		if IsIPv6(ip) {
			writeMasqRule(linesV6, chainV6, ip)
		} else {
			writeMasqRule(linesV4, chainV4, ip)
		}
	}

	writeLine(linesV4, "COMMIT")
	writeLine(linesV6, "COMMIT")

	if err := m.iptables.RestoreAll(linesV4.Bytes(), utiliptables.NoFlushTables, utiliptables.NoRestoreCounters); err != nil {
		return err
	}
	if err := m.ip6tables.RestoreAll(linesV6.Bytes(), utiliptables.NoFlushTables, utiliptables.NoRestoreCounters); err != nil {
		return err
	}
	return nil
}

func (m *MasqDaemon) syncMasqRulesIPv6(chain utiliptables.Chain) error {
	isIPv6Enabled := *enableIPv6

	if isIPv6Enabled {
		// make sure our custom chain for ipv6 non-masquerade exists
		_, err := m.ip6tables.EnsureChain(utiliptables.TableNAT, chain)
		if err != nil {
			return err
		}
		// ensure that any non-local in POSTROUTING jumps to chain
		if err := m.EnsurePostroutingJumpIPv6(chain); err != nil {
			return err
		}
		// build up lines to pass to ip6tables-restore
		lines6 := bytes.NewBuffer(nil)
		writeLine(lines6, "*nat")
		writeLine(lines6, utiliptables.MakeChainLine(chain)) // effectively flushes chain atomically with rule restore

		// link-local IPv6 CIDR is non-masquerade by default
		if !m.config.MasqLinkLocalIPv6 {
			writeNonMasqRule(lines6, linkLocalCIDRIPv6, chain)
		}

		for _, cidr := range m.config.NonMasqueradeCIDRs {
			if isIPv6CIDR(cidr) {
				writeNonMasqRule(lines6, cidr, chain)
			}
		}

		// masquerade all other traffic that is not bound for a --dst-type LOCAL destination
		writeMasqRule(lines6, chain, "anywhere")

		writeLine(lines6, "COMMIT")

		if err := m.ip6tables.RestoreAll(lines6.Bytes(), utiliptables.NoFlushTables, utiliptables.NoRestoreCounters); err != nil {
			return err
		}
	}
	return nil
}

// DeleteChain deletes chain. The error can be caused by either IPV4
// or IPV6.
func (m *MasqDaemon) DeleteChains(nsName string) error {
	chainV4 := utiliptables.Chain(ChainNameV4(nsName))
	retVal := m.iptables.FlushChain(utiliptables.TableNAT, chainV4)
	if err := m.iptables.DeleteChain(utiliptables.TableNAT, chainV4); err != nil {
		retVal = err
	}
	chainV6 := utiliptables.Chain(ChainNameV6(nsName))
	if err := m.ip6tables.FlushChain(utiliptables.TableNAT, chainV6); err != nil {
		retVal = err
	}
	if err := m.ip6tables.DeleteChain(utiliptables.TableNAT, chainV6); err != nil {
		retVal = err
	}
	return retVal
}

// NOTE(mtaufen): iptables requires names to be <= 28 characters, and somehow prepending "-m comment --comment " to this string makes it think this condition is violated
// Feel free to dig around in iptables and see if you can figure out exactly why; I haven't had time to fully trace how it parses and handle subcommands.
// If you want to investigate, get the source via `git clone git://git.netfilter.org/iptables.git`, `git checkout v1.4.21` (the version I've seen this issue on,
// though it may also happen on others), and start with `git grep XT_EXTENSION_MAXNAMELEN`.
func postroutingJumpComment(chain utiliptables.Chain) string {
	return fmt.Sprintf("purelb-egress: jump service endpoint traffic to our %s chain", chain)
}

func (m *MasqDaemon) EnsurePostroutingJump(chain utiliptables.Chain) error {
	if _, err := m.iptables.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, utiliptables.ChainPostrouting,
		"-m", "comment", "--comment", postroutingJumpComment(chain),
		"-m", "set", "--match-set", string(chain), "src", "-j", string(chain)); err != nil {
		return fmt.Errorf("failed to ensure that %s chain %s jumps to MASQUERADE: %v", utiliptables.TableNAT, chain, err)
	}
	return nil
}

func (m *MasqDaemon) DeletePostroutingJump(chain utiliptables.Chain) error {
	if err := m.iptables.DeleteRule(utiliptables.TableNAT, utiliptables.ChainPostrouting,
		"-m", "comment", "--comment", postroutingJumpComment(chain),
		"-m", "set", "--match-set", string(chain), "src", "-j", string(chain)); err != nil {
		return fmt.Errorf("failed to delete %s chain %s that jumps to MASQUERADE: %v", utiliptables.TableNAT, chain, err)
	}
	return nil
}

func (m *MasqDaemon) EnsurePostroutingJumpIPv6(chain utiliptables.Chain) error {
	if _, err := m.ip6tables.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, utiliptables.ChainPostrouting,
		"-m", "comment", "--comment", postroutingJumpComment(chain),
		"-m", "set", "--match-set", string(chain), "src", "-j", string(chain)); err != nil {
		return fmt.Errorf("failed to ensure that %s chain %s jumps to MASQUERADE: %v for ipv6", utiliptables.TableNAT, chain, err)
	}
	return nil
}

func (m *MasqDaemon) DeletePostroutingJumpIPv6(chain utiliptables.Chain) error {
	if err := m.ip6tables.DeleteRule(utiliptables.TableNAT, utiliptables.ChainPostrouting,
		"-m", "comment", "--comment", postroutingJumpComment(chain),
		"-m", "set", "--match-set", string(chain), "src", "-j", string(chain)); err != nil {
		return fmt.Errorf("failed to delete %s chain %s that jumps to MASQUERADE: %v", utiliptables.TableNAT, chain, err)
	}
	return nil
}

const nonMasqRuleComment = `-m comment --comment "local traffic is not subject to MASQUERADE"`

func writeNonMasqRule(lines *bytes.Buffer, cidr string, chain utiliptables.Chain) {
	writeRule(lines, utiliptables.Append, chain, nonMasqRuleComment, "-d", cidr, "-j", "RETURN")
}

const masqRuleComment = `-m comment --comment "outbound traffic is subject to MASQUERADE (must be last in chain)"`

func writeMasqRule(lines *bytes.Buffer, chain utiliptables.Chain, addr string) {
	writeRule(lines, utiliptables.Append, chain, masqRuleComment, "-j", "SNAT", "--to-source", addr)
}

// Similar syntax to utiliptables.Interface.EnsureRule, except you don't pass a table
// (you must write these rules under the line with the table name)
func writeRule(lines *bytes.Buffer, position utiliptables.RulePosition, chain utiliptables.Chain, args ...string) {
	fullArgs := append([]string{string(position), string(chain)}, args...)
	writeLine(lines, fullArgs...)
}

// Join all words with spaces, terminate with newline and write to buf.
func writeLine(lines *bytes.Buffer, words ...string) {
	lines.WriteString(strings.Join(words, " ") + "\n")
}

// isIPv6CIDR checks if the provided cidr block belongs to ipv6 family.
// If cidr belongs to ipv6 family, return true else it returns false
// which means the cidr belongs to ipv4 family
func isIPv6CIDR(cidr string) bool {
	ip, _, _ := net.ParseCIDR(cidr)
	return IsIPv6(ip.String())
}

// IsIPv6 checks if the provided ip belongs to ipv6 family.
// If ip belongs to ipv6 family, return true else it returns false
// which means the ip belongs to ipv4 family
func IsIPv6(ip string) bool {
	return net.ParseIP(ip).To4() == nil
}

func ChainNameV4(serviceName string) string {
	return CHAIN_PREFIX + chainName(serviceName) + "-" + IP_FAMILY_V4
}

func ChainNameV6(serviceName string) string {
	return CHAIN_PREFIX + chainName(serviceName) + "-" + IP_FAMILY_V6
}

// chainName takes serviceName and returns the associated 16 character
// hash. This is computed by hashing (sha256) then encoding to base32
// and truncating to 16 chars. We do this because IPTables Chain Names
// must be <= 28 chars long, and the longer they are the harder they
// are to read.
//
// This is lifted from proxier.go:portProtoHash() in the k8s source.
func chainName(serviceName string) string {
	hash := sha256.Sum256([]byte(serviceName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return encoded[:16]
}
