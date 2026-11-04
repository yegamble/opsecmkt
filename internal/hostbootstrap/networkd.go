package hostbootstrap

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

var errDHCPManaged = errors.New("DHCP managed interfaces are unsupported by this ingress profile")
var errNetworkdInventory = errors.New("supported host requires systemd-networkd static network inventory")

// networkdStaticLink judges `networkctl status --no-pager --no-legend --full
// --lines=0 NAME` text for one uplink (systemd 255 layout). It returns the
// .network file and drop-ins networkd applied, in order, so the caller can
// check their DHCP= setting. Any field or value mentioning DHCP is refused,
// except the DHCP6 Client DUID line, which networkd prints for every link
// (A-803), and the file paths, whose contents the caller reads instead.
// Output it cannot attribute to the named, networkd-managed link is refused.
func networkdStaticLink(name string, status []byte) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(string(status), "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return nil, errNetworkdInventory
	}
	// Header: "● 2: eth0" (UTF-8 glyph) or "* 2: eth0" (ASCII glyph).
	head := strings.Fields(lines[0])
	if len(head) == 3 {
		head = head[1:]
	}
	if len(head) != 2 || head[1] != name || !strings.HasSuffix(head[0], ":") {
		return nil, errNetworkdInventory
	}
	if _, e := strconv.ParseUint(strings.TrimSuffix(head[0], ":"), 10, 32); e != nil {
		return nil, errNetworkdInventory
	}
	var files []string
	sawNetwork := false
	key, valueCol := "", 0
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		var value string
		if valueCol > 0 && indent >= valueCol {
			value = strings.TrimSpace(line) // continuation of key's list
		} else if i := strings.Index(line, ": "); i > 0 && strings.TrimSpace(line[:i]) != "" {
			key, value, valueCol = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+2:]), i+2
			if key == "Network File" {
				if sawNetwork || value == "n/a" {
					return nil, errNetworkdInventory
				}
				sawNetwork = true
				files = append(files, value)
				continue
			}
		} else {
			return nil, errNetworkdInventory
		}
		switch {
		case key == "Network File":
			dropin := value
			for _, glyph := range []string{"├─", "└─", "|-", "`-"} {
				dropin = strings.TrimPrefix(dropin, glyph)
			}
			if dropin == value {
				return nil, errNetworkdInventory
			}
			files = append(files, dropin)
		case key == "Link File" || key == "DHCP6 Client DUID":
		case strings.Contains(strings.ToLower(key+" "+value), "dhcp"):
			return nil, errDHCPManaged
		}
	}
	if !sawNetwork {
		return nil, errNetworkdInventory
	}
	for _, f := range files {
		if !filepath.IsAbs(f) || filepath.Clean(f) != f {
			return nil, errNetworkdInventory
		}
	}
	return files, nil
}

// networkFileEnablesDHCP reports whether a .network file or drop-in assigns
// [Network] DHCP= anything but an explicit off value. Unknown values count as
// enabled; a later DHCP=no does not excuse an earlier enabling assignment.
func networkFileEnablesDHCP(content []byte) bool {
	section := ""
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			section = line
		case section == "[Network]":
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != "DHCP" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "", "no", "false", "0", "off":
			default:
				return true
			}
		}
	}
	return false
}
