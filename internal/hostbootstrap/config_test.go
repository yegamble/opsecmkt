package hostbootstrap

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func fixture() Config {
	return Config{Version: 1, OS: "ubuntu-24.04", Role: "app", Operator: "operator", SSHPort: 22, OperatorSources: []string{"192.0.2.10/32", "2001:db8::10/128"}, Uplinks: []string{"eth0", "eth1"}, AppAddress: "10.20.0.10", GatewayAddress: "10.20.0.20", AppPorts: []int{8080, 8443}}
}
func TestConfigRejectsUnsafeInventory(t *testing.T) {
	cases := map[string]func(*Config){"wide ssh": func(c *Config) { c.OperatorSources = []string{"0.0.0.0/0"} }, "mapped": func(c *Config) { c.OperatorSources = []string{"::ffff:192.0.2.10/128"} }, "root": func(c *Config) { c.Operator = "root" }, "injection": func(c *Config) { c.Uplinks = []string{"eth0\"; flush ruleset"} }, "public app": func(c *Config) { c.AppAddress = "192.0.2.1" }, "same peer": func(c *Config) { c.GatewayAddress = c.AppAddress }, "port": func(c *Config) { c.AppPorts = []int{0} }, "duplicate": func(c *Config) { c.AppPorts = []int{8080, 8080} }, "os": func(c *Config) { c.OS = "debian" }, "role": func(c *Config) { c.Role = "gateway" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixture()
			mutate(&c)
			if c.validate() == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
	c := fixture()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	for _, bad := range [][]byte{append(b, []byte(" {}")...), bytes.Replace(b, []byte(`"role":"app"`), []byte(`"role":"app","unknown":true`), 1)} {
		if _, err := decodeConfig(bad); err == nil {
			t.Fatal("accepted extra input")
		}
	}
}
func TestRulesProtectOriginalDockerDestinationAndBothFamilies(t *testing.T) {
	c := fixture()
	r := renderRules(c)
	for _, want := range []string{"hook input priority -10; policy drop", "hook forward priority -10", `iifname { "eth0", "eth1" }`, "ip saddr 10.20.0.20 ip daddr 10.20.0.10", "ct original ip daddr 10.20.0.10", "ct original proto-dst { 8080, 8443 }", "ct status dnat", "ip6 saddr 2001:db8::10", "nd-neighbor-solicit", "counter drop"} {
		if !strings.Contains(r, want) {
			t.Fatalf("missing %s", want)
		}
	}
	for _, bad := range []string{"flush ruleset", "tcp dport 80 ", "tcp dport 443 ", "hook output"} {
		if strings.Contains(r, bad) {
			t.Fatalf("unexpected %s", bad)
		}
	}
	c.AppAddress = "fd00::10"
	c.GatewayAddress = "fd00::20"
	if !strings.Contains(renderRules(c), "ct original ip6 daddr fd00::10") {
		t.Fatal("IPv6 DNAT missing")
	}
}
func TestFreshSessionRequired(t *testing.T) {
	c := fixture()
	old := "192.0.2.10 50000 10.20.0.10 22"
	for _, session := range []string{"", old, "192.0.2.11 50001 10.20.0.10 22", "192.0.2.10 50001 10.20.0.10 2222"} {
		if checkSession(c, session, old) == nil {
			t.Fatalf("accepted %q", session)
		}
	}
	if err := checkSession(c, "192.0.2.10 50001 10.20.0.10 22", old); err != nil {
		t.Fatal(err)
	}
}
