package market

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/keymaterial"
)

// A-589: the upload backend trusts only the gateway's client certificate. A
// certificate from the same UPLOAD_CA that lacks an explicit clientAuth EKU (Go
// treats no EKU as "any purpose") or that is itself a CA is refused with the
// same 404 as an unverified request, on every /v1/uploads/* route.
func TestUploadBackendRefusesSameCAClientCertsWithoutGatewayRole(t *testing.T) {
	d := t.TempDir()
	path := func(n string) string { return filepath.Join(d, n) }
	for _, args := range [][]string{
		{"generate", "tls-ca", "--name", "upload CA", "--cert-out", path("ca.pem"), "--key-out", path("ca.key")},
		{"generate", "tls-server", "--name", "backend", "--ca-cert", path("ca.pem"), "--ca-key", path("ca.key"), "--host", "127.0.0.1", "--cert-out", path("server.pem"), "--key-out", path("server.key")},
		{"generate", "tls-client", "--name", "gateway", "--ca-cert", path("ca.pem"), "--ca-key", path("ca.key"), "--cert-out", path("client.pem"), "--key-out", path("client.key")},
	} {
		if err := keymaterial.Run(args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	load := func(name string) (tls.Certificate, *x509.Certificate) {
		t.Helper()
		pair, err := tls.LoadX509KeyPair(path(name+".pem"), path(name+".key"))
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return pair, leaf
	}
	caPair, caCert := load("ca")
	serverPair, _ := load("server")
	gatewayPair, gatewayLeaf := load("client")
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	// mint issues a certificate under the same CA, as anyone holding the CA key
	// with another tool (or another service's issuance profile) might. It reuses
	// the gateway's key pair so only the certificate's role differs.
	mint := func(serial int64, tmpl *x509.Certificate) tls.Certificate {
		t.Helper()
		tmpl.SerialNumber = big.NewInt(serial)
		tmpl.Subject = pkix.Name{CommonName: "other service"}
		tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, gatewayLeaf.PublicKey, caPair.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: gatewayPair.PrivateKey}
	}
	noEKU := mint(100, &x509.Certificate{KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true})
	// The CA carries clientAuth so that only the IsCA refusal can reject it.
	subCA := mint(101, &x509.Certificate{KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, IsCA: true})

	a := &App{isolatedUploads: true}
	s := httptest.NewUnstartedServer(a.UploadBackendHandler())
	s.TLS = &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	s.StartTLS()
	t.Cleanup(s.Close)

	routes := []string{"/v1/uploads/admit", "/v1/uploads/finish", "/v1/uploads/branding-admit", "/v1/uploads/branding-finish", "/v1/uploads/staff-admit", gatewayScannerReportPath, "/v1/uploads/unknown"}
	post := func(cert tls.Certificate, route string) int {
		t.Helper()
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Post(s.URL+route, "text/plain", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: the TLS layer refused the certificate, so the handler check went untested: %v", route, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, cert := range map[string]tls.Certificate{"no-eku": noEKU, "ca-as-client": subCA} {
		for _, route := range routes {
			if code := post(cert, route); code != http.StatusNotFound {
				t.Errorf("%s on %s: HTTP %d, want 404", name, route, code)
			}
		}
	}
	// The documented `keys generate tls-client` gateway certificate still reaches
	// each route (the malformed body then gets that route's own 400).
	for _, route := range routes[:len(routes)-1] {
		if code := post(gatewayPair, route); code != http.StatusBadRequest {
			t.Errorf("gateway certificate on %s: HTTP %d, want 400", route, code)
		}
	}
}
