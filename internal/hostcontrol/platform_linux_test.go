//go:build linux

package hostcontrol

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This local socket test only reads kernel peer credentials. It never starts
// the root agent, schedules a timer or calls an operating-system executor.
func TestUnixPeerCredentialsComeFromKernel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, conn := range []net.Conn{client, server} {
		uid, err := unixPeerUID(conn)
		if err != nil || uid != uint32(os.Geteuid()) {
			t.Fatal("kernel UID not observed")
		}
		if err := verifyRootPeer(conn); (err == nil) != (os.Geteuid() == 0) {
			t.Fatal("client trusted a nonroot peer")
		}
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if _, err := unixPeerUID(left); err == nil {
		t.Fatal("non-Unix connection supplied peer proof")
	}
}

// Notification fixtures only receive a fixed readiness datagram. They do not
// start the privileged agent, install a service or invoke any reboot executor.
func TestSystemdReadinessNotificationUsesFixedBoundedDatagram(t *testing.T) {
	if err := notifySystemdReady(context.Background(), ""); err != nil {
		t.Fatal("manual run unexpectedly required notification")
	}
	for _, abstract := range []bool{false, true} {
		t.Run(fmt.Sprintf("abstract-%t", abstract), func(t *testing.T) {
			address := filepath.Join(t.TempDir(), "notify.sock")
			if abstract {
				address = fmt.Sprintf("@opsecmkt-notify-test-%d-%d", os.Getpid(), time.Now().UnixNano())
			}
			listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := notifySystemdReady(context.Background(), address); err != nil {
				t.Fatal(err)
			}
			var data [64]byte
			n, _, err := listener.ReadFromUnix(data[:])
			if err != nil || string(data[:n]) != "READY=1\n" {
				t.Fatal("fixed readiness notification was not received")
			}
		})
	}
}

func TestSystemdNotificationRefusesInvalidOrUnavailableAddresses(t *testing.T) {
	for _, address := range []string{"notify.sock", "/", "/run/../notify", "/run//notify", "@", "@bad\x00name", "@bad\nname", "/run/bad\tname", "@" + strings.Repeat("a", 107)} {
		if err := notifySystemdReady(context.Background(), address); err == nil || err.Error() != "host control service readiness notification failed" {
			t.Fatal("invalid notification address was accepted or exposed")
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.sock")
	if err := notifySystemdReady(context.Background(), missing); err == nil || strings.Contains(err.Error(), missing) {
		t.Fatal("missing notification socket did not fail safely")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := notifySystemdReady(ctx, "@opsecmkt-cancelled-notify-test"); err == nil {
		t.Fatal("cancelled startup sent readiness")
	}
}
