package hostbootstrap

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// staticEth0 is sanitised `networkctl status` output from an Ubuntu 24.04
// (systemd 255) host whose netplan .network file sets DHCP=no. networkd prints
// the machine-wide DHCP6 Client DUID line even for a static link (A-803).
const staticEth0 = `● 2: eth0
                   Link File: /run/systemd/network/10-netplan-eth0.link
                Network File: /run/systemd/network/10-netplan-eth0.network
                       State: routable (configured)
                Online state: online
                        Type: ether
                        Path: pci-0000:00:03.0
                      Driver: virtio_net
                      Vendor: Example Vendor
                       Model: Example Model
           Alternative Names: enp0s3
                              ens3
            Hardware Address: 52:54:00:00:00:01
                         MTU: 1500 (min: 68, max: 65535)
                       QDisc: pfifo_fast
IPv6 Address Generation Mode: eui64
    Number of Queues (Tx/Rx): 1/1
            Auto negotiation: no
                     Address: 192.0.2.10
                              2001:db8::10
                              fe80::5054:ff:fe00:1
                     Gateway: 192.0.2.1
                         DNS: 192.0.2.53
                              2001:db8::53
           Activation Policy: up
         Required For Online: yes
           DHCP6 Client DUID: DUID-EN/Vendor: 0000ab11aabbccddeeff0011
`

func TestNetworkdStaticLinkAcceptsStaticStatusWithGlobalDUID(t *testing.T) {
	files, err := networkdStaticLink("eth0", []byte(staticEth0))
	if err != nil {
		t.Fatalf("static link refused: %v", err)
	}
	if want := []string{"/run/systemd/network/10-netplan-eth0.network"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("network files = %q, want %q", files, want)
	}
	// The C locale glyphs and the drop-in tree are parsed too.
	ascii := strings.Replace(staticEth0, "● 2: eth0", "* 2: eth0", 1)
	ascii = strings.Replace(ascii, "10-netplan-eth0.network\n", "10-netplan-eth0.network\n"+
		"                              |-/etc/systemd/network/10-netplan-eth0.network.d/10-mtu.conf\n"+
		"                              `-/etc/systemd/network/10-netplan-eth0.network.d/20-dns.conf\n", 1)
	files, err = networkdStaticLink("eth0", []byte(ascii))
	if err != nil {
		t.Fatalf("ascii static link refused: %v", err)
	}
	if want := []string{"/run/systemd/network/10-netplan-eth0.network", "/etc/systemd/network/10-netplan-eth0.network.d/10-mtu.conf", "/etc/systemd/network/10-netplan-eth0.network.d/20-dns.conf"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("network files = %q, want %q", files, want)
	}
	utf := strings.Replace(ascii, "|-", "├─", 1)
	utf = strings.Replace(utf, "`-", "└─", 1)
	if got, err := networkdStaticLink("eth0", []byte(utf)); err != nil || !reflect.DeepEqual(got, files) {
		t.Fatalf("utf-8 drop-ins = %q, %v", got, err)
	}
}

func TestNetworkdStaticLinkRefusesDHCPInUse(t *testing.T) {
	for name, status := range map[string]string{
		"dhcp4 address":   strings.Replace(staticEth0, "Address: 192.0.2.10\n", "Address: 192.0.2.10 (DHCP4 via 192.0.2.1)\n", 1),
		"dhcpv4 address":  strings.Replace(staticEth0, "Address: 192.0.2.10\n", "Address: 192.0.2.10 (DHCPv4 via 192.0.2.1)\n", 1),
		"dhcp6 address":   strings.Replace(staticEth0, "  2001:db8::10\n", "  2001:db8::10 (DHCPv6 via fe80::1)\n", 1),
		"dhcp4 client id": strings.Replace(staticEth0, "           DHCP6 Client DUID:", "             DHCP4 Client ID: IAID:0x5c3a2b1/DUID\n           DHCP6 Client DUID:", 1),
		"dhcp6 lease":     strings.Replace(staticEth0, "           DHCP6 Client DUID:", "           DHCP6 Client IAID: 0x5c3a2b1\n           DHCP6 Client DUID:", 1),
		"dhcp server":     staticEth0 + "         Offered DHCP leases: 192.0.2.40 (to 01:52:54:00:00:00:02)\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := networkdStaticLink("eth0", []byte(status)); !errors.Is(err, errDHCPManaged) {
				t.Fatalf("got %v, want %v", err, errDHCPManaged)
			}
		})
	}
}

func TestNetworkdStaticLinkFailsClosedOnUnparseableStatus(t *testing.T) {
	for name, status := range map[string]string{
		"empty":            "",
		"blank":            "\n\n",
		"garbage":          "networkd says no\n",
		"other link":       strings.Replace(staticEth0, "● 2: eth0", "● 3: eth1", 1),
		"no header":        staticEth0[strings.Index(staticEth0, "\n")+1:],
		"unmanaged":        strings.Replace(staticEth0, "Network File: /run/systemd/network/10-netplan-eth0.network", "Network File: n/a", 1),
		"no network file":  strings.Replace(staticEth0, "                Network File: /run/systemd/network/10-netplan-eth0.network\n", "", 1),
		"relative file":    strings.Replace(staticEth0, "Network File: /run/systemd/network/", "Network File: ../", 1),
		"stray text line":  staticEth0 + "unexpected\n",
		"bad drop-in line": strings.Replace(staticEth0, "10-netplan-eth0.network\n", "10-netplan-eth0.network\n                              conf.d/x.conf\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := networkdStaticLink("eth0", []byte(status)); !errors.Is(err, errNetworkdInventory) {
				t.Fatalf("got %v, want %v", err, errNetworkdInventory)
			}
		})
	}
}

func TestNetworkFileEnablesDHCP(t *testing.T) {
	for content, want := range map[string]bool{
		"[Match]\nName=eth0\n\n[Network]\nDHCP=no\nAddress=192.0.2.10/24\n": false,
		"[Network]\nAddress=192.0.2.10/24\n":                                false,
		"[Network]\nDHCP=false\nDHCP=\n":                                    false,
		"[DHCPv4]\nDHCP=yes\n[Network]\nDHCP=off\n":                         false,
		"# DHCP=yes\n[Network]\n; DHCP=yes\nDHCP=0\n":                       false,
		"[Network]\nAddress=192.0.2.10/24\nDHCP=yes\n":                      true,
		"[Network]\nDHCP = ipv4\n":                                          true,
		"[Network]\nDHCP=ipv6\n":                                            true,
		"[Network]\r\nDHCP=true\r\n":                                        true,
		"[Network]\nDHCP=\\\n  yes\n":                                       true,
		"[Network]\nDHCP=no\n[Network]\nDHCP=both\n":                        true,
		"[Network]\nDHCP=maybe\n":                                           true,
	} {
		if got := networkFileEnablesDHCP([]byte(content)); got != want {
			t.Errorf("networkFileEnablesDHCP(%q) = %v, want %v", content, got, want)
		}
	}
}

// The pre-A-803 check refused any status mentioning DHCP; keep that decision
// reproducible so the static sample above is a real regression test.
func TestNetworkdStaticLinkRegressionAgainstSubstringCheck(t *testing.T) {
	if !strings.Contains(strings.ToLower(staticEth0), "dhcp") {
		t.Fatal("sample no longer exercises the global DHCP6 Client DUID line")
	}
}
