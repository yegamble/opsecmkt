package keymaterial

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"time"
)

func tlsOutputs(action, kind string, o options) ([]outputFile, error) {
	if action == "import" {
		certPEM, err := readFile(o.certIn, false)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readFile(o.keyIn, true)
		if err != nil {
			return nil, err
		}
		if _, _, err = certificatePair(certPEM, keyPEM); err != nil {
			return nil, err
		}
		return []outputFile{{o.certOut, certPEM, 0644}, {o.keyOut, keyPEM, 0600}}, nil
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("keys: TLS key generation failed")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, errors.New("keys: TLS serial generation failed")
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: o.name}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(time.Duration(o.days) * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	var parent *x509.Certificate
	var signer crypto.Signer
	if kind == "tls-ca" {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		template.MaxPathLen = 0
		template.MaxPathLenZero = true
		parent, signer = template, private
	} else {
		certPEM, err := readFile(o.caCert, false)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readFile(o.caKey, true)
		if err != nil {
			return nil, err
		}
		parent, signer, err = certificatePair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		if !parent.IsCA || parent.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, errors.New("keys: issuer certificate is not a certificate authority")
		}
		if template.NotAfter.After(parent.NotAfter) {
			template.NotAfter = parent.NotAfter
		}
		if kind == "tls-server" {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			for _, host := range strings.Split(o.hosts, ",") {
				host = strings.TrimSpace(host)
				if ip := net.ParseIP(host); ip != nil {
					template.IPAddresses = append(template.IPAddresses, ip)
				} else {
					if !validDNSName(host) {
						return nil, errors.New("keys: server hosts must be DNS names or IP addresses without schemes or ports")
					}
					template.DNSNames = append(template.DNSNames, host)
				}
			}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		return nil, errors.New("keys: TLS certificate signing failed")
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, errors.New("keys: TLS private key serialization failed")
	}
	return []outputFile{
		{o.certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644},
		{o.keyOut, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}), 0600},
	}, nil
}

func certificatePair(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	// The standard parser validates that the leaf and private key match. Also
	// reject junk/trailing PEM rather than silently importing ambiguous bundles.
	rest := certPEM
	var certificates []*x509.Certificate
	for len(strings.TrimSpace(string(rest))) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 || !bytesHasPEMPrefix(rest, "CERTIFICATE") {
			return nil, nil, errors.New("keys: invalid TLS certificate PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, errors.New("keys: invalid TLS certificate")
		}
		certificates = append(certificates, cert)
		rest = remaining
	}
	keyBlock, keyRest := pem.Decode(keyPEM)
	if keyBlock == nil || !bytesHasPEMPrefix(keyPEM, keyBlock.Type) || len(strings.TrimSpace(string(keyRest))) > 0 || len(keyBlock.Headers) > 0 {
		return nil, nil, errors.New("keys: invalid or encrypted TLS private key PEM")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(certificates) == 0 {
		return nil, nil, errors.New("keys: TLS certificate and private key are invalid or do not match")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("keys: unsupported TLS private key")
	}
	now := time.Now()
	for _, cert := range certificates {
		if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return nil, nil, errors.New("keys: TLS certificate is expired or not yet valid")
		}
	}
	return certificates[0], signer, nil
}

func bytesHasPEMPrefix(data []byte, kind string) bool {
	return strings.HasPrefix(strings.TrimSpace(string(data)), "-----BEGIN "+kind+"-----")
}

func validDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
