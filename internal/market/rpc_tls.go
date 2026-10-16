package market

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
)

// moneroRPCTLS supports private service PKI without disabling hostname validation.
// The optional client identity authenticates the app to a dedicated RPC proxy.
func moneroRPCTLS(getenv func(string) string) (*tls.Config, error) {
	ca, cert, key := getenv("MONERO_RPC_CA"), getenv("MONERO_RPC_CERT"), getenv("MONERO_RPC_KEY")
	if ca == "" && cert == "" && key == "" {
		return nil, nil
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		f, err := os.Open(ca)
		if err != nil {
			return nil, errors.New("MONERO_RPC_CA is unreadable")
		}
		b, err := io.ReadAll(io.LimitReader(f, 65537))
		f.Close()
		pool := x509.NewCertPool()
		if err != nil || len(b) > 65536 || !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("MONERO_RPC_CA must contain a PEM CA (maximum 64 KiB)")
		}
		c.RootCAs = pool
	}
	if cert != "" || key != "" {
		if cert == "" || key == "" {
			return nil, errors.New("MONERO_RPC_CERT and MONERO_RPC_KEY must be set together")
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, errors.New("cannot load MONERO_RPC_CERT/MONERO_RPC_KEY")
		}
		c.Certificates = []tls.Certificate{pair}
	}
	return c, nil
}
