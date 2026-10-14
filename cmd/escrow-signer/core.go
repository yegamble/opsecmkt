package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// core is a minimal Bitcoin Core JSON-RPC client for the signer's own RPC user, which the operator whitelists to exactly
// what it calls: decodepsbt, descriptorprocesspsbt and getdescriptorinfo (docs/operator-guide.md, "Escrow signer").
// Every failure is one of two fixed errors; Core's error text is never kept, logged or returned, because it could quote
// the descriptor and with it the account tprv. Requests go directly to the configured node and never follow redirects:
// their bodies carry account private keys, which must not reach an environment proxy or another endpoint.
type core struct {
	endpoint   string // the URL without its user information
	user, pass string
	client     *http.Client
}

var (
	errCoreDown    = errors.New("the escrow node did not answer")   // transport failure, timeout, HTTP 401/403, or a reply that is not JSON-RPC
	errCoreRefused = errors.New("the escrow node refused the call") // a JSON-RPC error
	errCoreURL     = errors.New("ESCROW_SIGNER_RPC_URL must be http(s)://user:password@host:port, for the signer's own RPC user")
)

// newCore parses ESCROW_SIGNER_RPC_URL. Its error never repeats the URL (it holds the password).
func newCore(raw string) (*core, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User == nil {
		return nil, errCoreURL
	}
	pass, ok := u.User.Password()
	if !ok || u.User.Username() == "" || pass == "" {
		return nil, errCoreURL
	}
	user := u.User.Username()
	u.User = nil
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &core{endpoint: u.String(), user: user, pass: pass, client: &http.Client{
		Timeout:       2 * time.Minute,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// call runs one JSON-RPC 1.0 call and decodes its result into out (json.Number for numbers when out is *any).
func (c *core) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "escrow-signer", "method": method, "params": params})
	if err != nil {
		return errCoreRefused
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return errCoreDown
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return errCoreDown
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return errCoreDown
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int64 `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return errCoreDown // 401, 403 (not whitelisted) and proxies answer no JSON-RPC body
	}
	if reply.Error != nil {
		return errCoreRefused
	}
	if resp.StatusCode != http.StatusOK {
		return errCoreDown
	}
	dec := json.NewDecoder(bytes.NewReader(reply.Result))
	dec.UseNumber()
	if dec.Decode(out) != nil {
		return errCoreDown
	}
	return nil
}
