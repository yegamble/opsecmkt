// Package hostbootstrap manages an explicit application-host ingress policy.
// It never provisions remote services, rotates identities or starts the app.
package hostbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

const table = "opsecmkt_host"

// Config is a portable, non-secret inventory for a dedicated static-address host.
type Config struct {
	Version         int      `json:"version"`
	OS              string   `json:"os"`
	Role            string   `json:"role"`
	Operator        string   `json:"operator"`
	SSHPort         int      `json:"ssh_port"`
	OperatorSources []string `json:"operator_sources"`
	Uplinks         []string `json:"uplinks"`
	AppAddress      string   `json:"app_address"`
	GatewayAddress  string   `json:"gateway_address"`
	AppPorts        []int    `json:"app_ports"`
}

var interfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,14}$`)
var userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func decodeConfig(b []byte) (Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("host configuration must contain exactly one JSON object with known fields")
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if c.Version != 1 || c.OS != "ubuntu-24.04" || c.Role != "app" {
		return errors.New("host bootstrap supports version 1, ubuntu-24.04, role app only")
	}
	if !userName.MatchString(c.Operator) || c.Operator == "root" {
		return errors.New("operator must be a named nonroot Linux account")
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 || len(c.OperatorSources) == 0 || len(c.OperatorSources) > 16 || len(c.Uplinks) == 0 || len(c.Uplinks) > 16 || len(c.AppPorts) == 0 || len(c.AppPorts) > 8 {
		return errors.New("invalid or missing host ports, operator sources or uplinks")
	}
	seen := map[string]bool{}
	for _, s := range c.OperatorSources {
		p, e := netip.ParsePrefix(s)
		if e != nil || p.Bits() != p.Addr().BitLen() || !p.Addr().IsGlobalUnicast() || p.Addr().Is4In6() || p.Addr().IsLoopback() || p.String() != s || seen[s] {
			return errors.New("operator sources must be unique canonical individual /32 or /128 addresses")
		}
		seen[s] = true
	}
	for _, s := range c.Uplinks {
		if !interfaceName.MatchString(s) || s == "lo" || seen["if:"+s] {
			return errors.New("uplinks must be unique explicit Linux interface names")
		}
		seen["if:"+s] = true
	}
	app, e := netip.ParseAddr(c.AppAddress)
	if e != nil || !app.IsPrivate() || app.Is4In6() || app.String() != c.AppAddress {
		return errors.New("app address must be a canonical private IP")
	}
	gateway, e := netip.ParseAddr(c.GatewayAddress)
	if e != nil || !gateway.IsPrivate() || gateway.Is4In6() || gateway.String() != c.GatewayAddress || gateway == app || gateway.BitLen() != app.BitLen() {
		return errors.New("gateway must have a distinct canonical private IP in the app address family")
	}
	for _, port := range c.AppPorts {
		key := "port:" + strconv.Itoa(port)
		if port < 1 || port > 65535 || port == c.SSHPort || seen[key] {
			return errors.New("application ports must be unique valid TCP ports distinct from SSH")
		}
		seen[key] = true
	}
	return nil
}
func renderRules(c Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n chain input {\n  type filter hook input priority -10; policy drop;\n  ct state invalid drop\n  iifname lo accept\n  ct state established,related accept\n  ip protocol icmp icmp type { destination-unreachable, time-exceeded, parameter-problem } accept\n  meta l4proto 58 icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept\n  ip6 hoplimit 255 icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n", table)
	for _, s := range c.OperatorSources {
		p, _ := netip.ParsePrefix(s)
		family := "ip"
		if p.Addr().Is6() {
			family = "ip6"
		}
		fmt.Fprintf(&b, "  %s saddr %s tcp dport %d counter accept\n", family, p.Addr(), c.SSHPort)
	}
	family := "ip"
	if strings.Contains(c.AppAddress, ":") {
		family = "ip6"
	}
	ports := make([]string, len(c.AppPorts))
	for i, p := range c.AppPorts {
		ports[i] = strconv.Itoa(p)
	}
	fmt.Fprintf(&b, "  %s saddr %s %s daddr %s tcp dport { %s } counter accept\n  counter drop\n }\n", family, c.GatewayAddress, family, c.AppAddress, strings.Join(ports, ", "))
	uplinks := make([]string, len(c.Uplinks))
	for i, n := range c.Uplinks {
		uplinks[i] = strconv.Quote(n)
	}
	fmt.Fprintf(&b, " chain forward {\n  type filter hook forward priority -10; policy accept;\n  ct state invalid drop\n  ct state established,related accept\n  iifname { %s } jump inbound_publish\n }\n chain inbound_publish {\n  meta l4proto tcp %s saddr %s ct status dnat ct original %s daddr %s ct original proto-dst { %s } counter accept\n  counter drop\n }\n}\n", strings.Join(uplinks, ", "), family, c.GatewayAddress, family, c.AppAddress, strings.Join(ports, ", "))
	return b.String()
}
func checkSession(c Config, current, previous string) error {
	fields := strings.Fields(current)
	if len(fields) != 4 || current == previous {
		return errors.New("confirmation requires a fresh approved SSH connection; preserve SSH_CONNECTION through sudo")
	}
	source, e := netip.ParseAddr(fields[0])
	if e != nil {
		return errors.New("invalid SSH connection source")
	}
	dest, e := netip.ParseAddr(fields[2])
	if e != nil || !dest.IsGlobalUnicast() {
		return errors.New("invalid SSH connection destination")
	}
	sourcePort, e := strconv.Atoi(fields[1])
	if e != nil || sourcePort < 1 || sourcePort > 65535 {
		return errors.New("invalid SSH connection source port")
	}
	port, e := strconv.Atoi(fields[3])
	if e != nil || port != c.SSHPort {
		return errors.New("SSH connection port does not match inventory")
	}
	for _, s := range c.OperatorSources {
		p, _ := netip.ParsePrefix(s)
		if p.Contains(source) {
			return nil
		}
	}
	return errors.New("SSH connection source is not an approved operator address")
}
