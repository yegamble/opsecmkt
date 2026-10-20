package market

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

type xmppTestResult struct {
	authenticated bool
	message       *xmppElement
	err           error
}

func xmppTestCertificate(t *testing.T, wrongHost bool) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "XMPP local test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if wrongHost {
		template.IPAddresses = nil
		template.DNSNames = []string{"wrong.example.test"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// This fake server checks the wire conversation and uses a real certificate-
// verified TLS connection; no production verification bypass is involved.
func xmppTestServer(t *testing.T, mode, behavior string) (xmppConfig, *x509.CertPool, <-chan xmppTestResult) {
	t.Helper()
	cert, roots := xmppTestCertificate(t, behavior == "wrong-host")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan xmppTestResult, 1)
	go func() {
		result := xmppTestResult{}
		defer func() { done <- result }()
		conn, err := listener.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		if behavior == "stall" {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		upgrade := func() bool {
			secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err := secured.Handshake(); err != nil {
				result.err = err
				return false
			}
			conn = secured
			return true
		}
		if mode == "direct" && !upgrade() {
			return
		}
		stream := &xmppStream{conn: conn, dec: xml.NewDecoder(conn)}
		write := func(s string) bool {
			if err := stream.write(s); err != nil {
				result.err = err
				return false
			}
			return true
		}
		open := func(features string) bool {
			start, err := stream.start()
			if err != nil || start.Name != (xml.Name{Space: xmppStreamNS, Local: "stream"}) {
				result.err = fmt.Errorf("client stream: %v", err)
				return false
			}
			return write("<stream:stream xmlns='jabber:client' xmlns:stream='" + xmppStreamNS + "' from='example.test' version='1.0'><stream:features>" + features + "</stream:features>")
		}
		expect := func(space, local string) *xmppElement {
			e, err := stream.element()
			if err != nil || e.Name != (xml.Name{Space: space, Local: local}) {
				result.err = fmt.Errorf("wanted %s: %v", local, err)
				return nil
			}
			return e
		}
		if mode == "starttls" {
			if behavior == "no-tls" {
				if !open("<mechanisms xmlns='" + xmppSASLNS + "'><mechanism>PLAIN</mechanism></mechanisms>") {
					return
				}
				// No credential-bearing bytes may follow a plaintext-only offer.
				payload, _ := io.ReadAll(conn)
				if len(payload) != 0 {
					result.err = fmt.Errorf("client sent %d bytes after refusing plaintext", len(payload))
				}
				return
			}
			if !open("<starttls xmlns='"+xmppTLSNS+"'><required/></starttls>") || expect(xmppTLSNS, "starttls") == nil {
				return
			}
			if behavior == "tls-refused" {
				write("<failure xmlns='" + xmppTLSNS + "'/>")
				return
			}
			if !write("<proceed xmlns='"+xmppTLSNS+"'/>") || !upgrade() {
				return
			}
			stream = &xmppStream{conn: conn, dec: xml.NewDecoder(conn)}
		}
		features := "<mechanisms xmlns='" + xmppSASLNS + "'><mechanism>PLAIN</mechanism></mechanisms>"
		switch behavior {
		case "no-plain":
			features = "<mechanisms xmlns='" + xmppSASLNS + "'><mechanism>ANONYMOUS</mechanism></mechanisms>"
		case "malformed":
			features = "<broken>"
		case "oversized":
			features = "<padding>" + strings.Repeat("x", 300*1024) + "</padding>"
		case "deep":
			features = strings.Repeat("<n>", 20) + strings.Repeat("</n>", 20)
		}
		if !open(features) {
			return
		}
		if behavior == "no-plain" || behavior == "malformed" || behavior == "oversized" || behavior == "deep" {
			return
		}
		auth := expect(xmppSASLNS, "auth")
		if auth == nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(auth.Text)
		if err != nil || string(decoded) != "\x00outgoing\x00test-password" || auth.attr("mechanism") != "PLAIN" {
			result.err = errors.New("unexpected SASL credentials")
			return
		}
		result.authenticated = true
		if behavior == "auth-refused" {
			write("<failure xmlns='" + xmppSASLNS + "'><not-authorized/><text>secret-sensitive-server-detail</text></failure>")
			return
		}
		if !write("<success xmlns='" + xmppSASLNS + "'/>") {
			return
		}
		stream = &xmppStream{conn: conn, dec: xml.NewDecoder(conn)}
		features = "<bind xmlns='" + xmppBindNS + "'/>"
		if behavior == "session" {
			features += "<session xmlns='urn:ietf:params:xml:ns:xmpp-session'/>"
		}
		if !open(features) {
			return
		}
		bind := expect(xmppClientNS, "iq")
		if bind == nil || bind.child(xmppBindNS, "bind") == nil {
			result.err = errors.New("no resource binding requested")
			return
		}
		if behavior == "bind-refused" {
			write("<iq id='xmpp-bind' type='error'/>")
			return
		}
		boundJID := "outgoing@example.test/market-notifications"
		if behavior == "bind-casefold" {
			boundJID = "OUTGOING@EXAMPLE.TEST/server-generated-resource"
		}
		if behavior == "wrong-bound-jid" {
			boundJID = "another@example.test/market-notifications"
		}
		if !write("<iq id='xmpp-bind' type='result'><bind xmlns='" + xmppBindNS + "'><jid>" + boundJID + "</jid></bind></iq>") {
			return
		}
		if behavior == "wrong-bound-jid" {
			return
		}
		if behavior == "session" {
			session := expect(xmppClientNS, "iq")
			if session == nil || session.attr("id") != "xmpp-session" || !write("<iq id='xmpp-session' type='result'/>") {
				return
			}
		}
		result.message = expect(xmppClientNS, "message")
		if result.message == nil {
			return
		}
		ping := expect(xmppClientNS, "iq")
		if ping == nil || ping.attr("id") != "xmpp-submitted" || ping.child("urn:xmpp:ping", "ping") == nil {
			result.err = errors.New("no server submission check requested")
			return
		}
		switch behavior {
		case "message-refused":
			write("<message type='error' id='" + xmppEscape(result.message.attr("id")) + "'><error type='cancel'/></message><iq id='xmpp-submitted' type='result'/>")
		case "noisy":
			write(strings.Repeat("<presence/>", 40) + "<iq id='xmpp-submitted' type='result'/>")
		case "ping-unsupported":
			write("<iq id='xmpp-submitted' type='error'><error type='cancel'><service-unavailable xmlns='urn:ietf:params:xml:ns:xmpp-stanzas'/></error></iq>")
		default:
			write("<presence/><iq id='xmpp-submitted' type='result'/>")
		}
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	portNumber, _ := strconv.Atoi(port)
	return xmppConfig{Enabled: true, Sender: "outgoing@example.test", Host: "127.0.0.1", Port: portNumber, TLSMode: mode, Password: "test-password"}, roots, done
}

func TestXMPPTransport(t *testing.T) {
	for _, mode := range []string{"direct", "starttls"} {
		for _, behavior := range []string{"success", "session", "bind-casefold", "ping-unsupported", "auth-refused", "no-plain", "malformed", "oversized", "deep", "bind-refused", "wrong-bound-jid", "message-refused", "noisy"} {
			t.Run(mode+"/"+behavior, func(t *testing.T) {
				cfg, roots, done := xmppTestServer(t, mode, behavior)
				const body = "Order 123: Payment confirmed. <safe> & \"quoted\" 'text'\n新しい通知"
				err := sendXMPPWithRoots(context.Background(), cfg, "buyer@example.test", "market-test&'id", body, roots)
				success := behavior == "success" || behavior == "session" || behavior == "bind-casefold" || behavior == "ping-unsupported"
				if (err == nil) != success {
					t.Fatalf("success=%v error=%v", success, err)
				}
				if err != nil && strings.Contains(err.Error(), "secret-sensitive") {
					t.Fatal("server text leaked into error")
				}
				result := <-done
				if success {
					if result.err != nil || !result.authenticated || result.message == nil {
						t.Fatalf("server conversation failed: %v", result.err)
					}
					if result.message.attr("to") != "buyer@example.test" || result.message.attr("id") != "market-test&'id" || result.message.attr("type") != "chat" || result.message.child(xmppClientNS, "body").Text != body {
						t.Fatalf("message changed during delivery: %#v", result.message)
					}
				}
			})
		}
	}
}

func TestXMPPTransportRefusesTLSFailuresAndDowngrade(t *testing.T) {
	for _, test := range []struct{ mode, behavior string }{{"starttls", "no-tls"}, {"starttls", "tls-refused"}, {"starttls", "untrusted"}, {"direct", "untrusted"}, {"starttls", "wrong-host"}, {"direct", "wrong-host"}} {
		t.Run(test.mode+"/"+test.behavior, func(t *testing.T) {
			cfg, roots, done := xmppTestServer(t, test.mode, test.behavior)
			if test.behavior != "wrong-host" {
				roots = nil
			}
			if err := sendXMPPWithRoots(context.Background(), cfg, "buyer@example.test", "id", "Notice", roots); !errors.Is(err, errXMPPTLS) {
				t.Fatalf("got %v; want TLS failure", err)
			}
			result := <-done
			if result.authenticated || result.message != nil {
				t.Fatal("credentials or message submitted without verified TLS")
			}
			if test.behavior == "no-tls" && result.err != nil {
				t.Fatal(result.err)
			}
		})
	}
}

func TestXMPPTransportCancellationInterruptsStalledServer(t *testing.T) {
	cfg, roots, done := xmppTestServer(t, "starttls", "stall")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	if err := sendXMPPWithRoots(ctx, cfg, "buyer@example.test", "id", "Notice", roots); err == nil {
		t.Fatal("stalled server reported success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	<-done
}

func TestXMPPAddressAndConfigurationValidation(t *testing.T) {
	for _, address := range []string{"alice@example.test", "User.Name+tag@xmpp.example.test", "a@localhost"} {
		if !validateXMPPJID(address) {
			t.Errorf("rejected address %q", address)
		}
	}
	for _, address := range []string{"", "a", "@example.test", "a@", "a@b@c", "a@example.test/resource", "a@example.test\n", " a@example.test", "a b@example.test", "a'@example.test", "a@-example.test", "a@example..test", "a@example.test.", "a@[127.0.0.1]", "a\\20b@example.test", "用戶@example.test", strings.Repeat("a", 65) + "@example.test"} {
		if validateXMPPJID(address) {
			t.Errorf("accepted invalid address %q", address)
		}
	}
	cfg := xmppConfig{Sender: "outgoing@example.test", Host: "localhost", Port: 5222, TLSMode: "starttls", Password: "test-password"}
	if !cfg.valid() {
		t.Fatal("valid configuration refused")
	}
	for _, invalid := range []xmppConfig{
		{Sender: cfg.Sender, Host: cfg.Host, Port: 5222, TLSMode: "plaintext", Password: cfg.Password},
		{Sender: cfg.Sender, Host: "https://example.test", Port: 5222, TLSMode: cfg.TLSMode, Password: cfg.Password},
		{Sender: cfg.Sender, Host: cfg.Host, Port: 0, TLSMode: cfg.TLSMode, Password: cfg.Password},
		{Sender: cfg.Sender, Host: cfg.Host, Port: 65536, TLSMode: cfg.TLSMode, Password: cfg.Password},
		{Sender: cfg.Sender, Host: cfg.Host, Port: cfg.Port, TLSMode: cfg.TLSMode, Password: "password\x00invalid"},
	} {
		if err := sendXMPP(context.Background(), invalid, "buyer@example.test", "id", "Notice"); !errors.Is(err, errXMPPConfig) {
			t.Errorf("invalid configuration got %v", err)
		}
	}
	for _, body := range []string{"bad\x00text", "bad\x1ftext", string([]byte{0xff}), strings.Repeat("a", xmppMaxBodyBytes+1)} {
		if err := sendXMPP(context.Background(), cfg, "buyer@example.test", "id", body); !errors.Is(err, errXMPPConfig) {
			t.Errorf("invalid body got %v", err)
		}
	}
}
