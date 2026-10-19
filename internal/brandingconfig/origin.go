// Package brandingconfig validates fixed browser-facing origins from operator
// configuration: the public upload gateway that administrator-authorized logo
// uploads are handed to, and the market's official addresses (A-572).
package brandingconfig

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"opsecmkt/internal/onionidentity"
)

const originError = "BRANDING_UPLOAD_ORIGIN must be an HTTPS origin or an HTTP v3 onion origin, without credentials, path, query or fragment; HTTP loopback is development-only"

const officialAddressesError = "OFFICIAL_ADDRESSES must be a comma-separated list of at most 8 HTTPS origins or HTTP v3 onion origins, without credentials, path, query or fragment; HTTP loopback is development-only"

// MaxOfficialAddresses bounds OFFICIAL_ADDRESSES so the list stays readable in a sign-in message.
const MaxOfficialAddresses = 8

// ParseOfficialAddresses accepts OFFICIAL_ADDRESSES, the market's public addresses (its onion, mirror and clearnet
// origins) as a comma-separated list, and returns their canonical forms in order without duplicates. Each entry
// follows ParseOrigin's rules; blank means none is configured. The error never repeats the input.
func ParseOfficialAddresses(raw string, development bool) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		origin, err := ParseOrigin(strings.Trim(entry, " "), development)
		if err != nil || origin == "" {
			return nil, errors.New(officialAddressesError)
		}
		if !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	if len(out) > MaxOfficialAddresses {
		return nil, errors.New(officialAddressesError)
	}
	return out, nil
}

// ParseOrigin accepts an optional origin and returns its canonical browser-facing
// form. It performs no lookup or request. Blank preserves existing deployments;
// callers decide whether their upload topology can operate without a handoff.
func ParseOrigin(raw string, development bool) (string, error) {
	if raw == "" {
		return "", nil
	}
	refuse := func() (string, error) { return "", errors.New(originError) }
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\x00\r\n\t") {
		return refuse()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return refuse()
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if scheme != "https" && scheme != "http" || host == "" || strings.ContainsAny(host, "% /\\") {
		return refuse()
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return refuse()
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return refuse()
		}
		port = strconv.Itoa(value)
		if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
			port = ""
		}
	}
	ip := net.ParseIP(host)
	if strings.ContainsAny(u.Host, "[]") && (ip == nil || !strings.Contains(host, ":")) {
		return refuse()
	}
	if strings.HasSuffix(host, ".onion") {
		if onionidentity.ValidateAddress(host) != nil {
			return refuse()
		}
	} else if ip == nil && !dnsHost(host) {
		return refuse()
	}
	if scheme == "http" && !strings.HasSuffix(host, ".onion") && !(development && (host == "localhost" || ip != nil && ip.IsLoopback())) {
		return refuse()
	}
	if ip != nil {
		host = ip.String()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// Use ASCII DNS labels rather than browser-specific host coercions such as
// integer IPv4, shortened IPv4, percent escapes, IDNA mappings or trailing dots.
func dnsHost(host string) bool {
	if len(host) > 253 || strings.Trim(host, "0123456789.") == "" {
		return false
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		if _, err := strconv.ParseUint(last[2:], 16, 64); err == nil {
			return false
		}
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
