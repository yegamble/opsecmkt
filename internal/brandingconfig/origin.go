// Package brandingconfig validates the fixed browser-facing origin used to hand
// administrator-authorized logo uploads to the public upload gateway.
package brandingconfig

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"opsecmkt/internal/onionidentity"
)

const originError = "BRANDING_UPLOAD_ORIGIN must be an HTTPS origin or an HTTP v3 onion origin, without credentials, path, query or fragment; HTTP loopback is development-only"

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
