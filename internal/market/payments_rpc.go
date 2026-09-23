package market

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// rpcClient is a minimal JSON-RPC client for Bitcoin Core (1.0, HTTP Basic auth) and monero-wallet-rpc /
// monerod (2.0, HTTP Digest MD5 qop=auth). Credentials come from the URL userinfo and are removed from the
// endpoint, so they never appear in errors or logs. No proxy, no redirects, 10 s timeout per call.
type rpcClient struct {
	name       string // "bitcoin", "monero wallet", "monero daemon" (for errors)
	endpoint   string // scheme://host[:port][/path] without userinfo
	user, pass string
	hasAuth    bool
	digest     bool
	version    string // "1.0" or "2.0"
	http       *http.Client

	mu        sync.Mutex
	challenge map[string]string // last Digest challenge
	nc        int
}

const rpcTimeout = 10 * time.Second
const rpcMaxBody = 16 << 20

type rpcError struct {
	Method  string
	Code    int64
	Message string
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("%s: error %d: %s", e.Method, e.Code, e.Message)
}

func newRPCClient(name, raw, version string, digest bool) (*rpcClient, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s RPC URL must be http(s)://[user:password@]host:port", name)
	}
	c := &rpcClient{name: name, version: version, digest: digest}
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
		c.hasAuth = true
	}
	u.User = nil
	u.RawQuery, u.Fragment = "", ""
	c.endpoint = strings.TrimRight(u.String(), "/")
	c.http = &http.Client{
		Timeout:       rpcTimeout,
		Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: rpcTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// call posts one request to endpoint+path and decodes "result" into out (numbers as json.Number).
func (c *rpcClient) call(ctx context.Context, path, method string, params, out any) error {
	req := map[string]any{"jsonrpc": c.version, "id": "opsecmkt", "method": method}
	if params != nil {
		req["params"] = params
	} else if c.version == "1.0" {
		req["params"] = []any{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := c.send(ctx, path, body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.digest {
		challenge := parseDigestChallenge(resp.Header.Values("WWW-Authenticate"))
		resp.Body.Close()
		if challenge == nil {
			return fmt.Errorf("%s RPC %s: authentication required", c.name, method)
		}
		c.mu.Lock()
		c.challenge, c.nc = challenge, 0
		c.mu.Unlock()
		if resp, err = c.send(ctx, path, body); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s RPC %s: credentials rejected (HTTP %d)", c.name, method, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, rpcMaxBody+1))
	if err != nil {
		return fmt.Errorf("%s RPC %s: reading response failed", c.name, method)
	}
	if len(raw) > rpcMaxBody {
		return fmt.Errorf("%s RPC %s: response too large", c.name, method)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    json.Number `json:"code"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&env); err != nil {
		return fmt.Errorf("%s RPC %s: invalid response (HTTP %d)", c.name, method, resp.StatusCode)
	}
	if env.Error != nil {
		code, _ := env.Error.Code.Int64()
		return &rpcError{Method: c.name + " RPC " + method, Code: code, Message: truncate(env.Error.Message, 200)}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s RPC %s: HTTP %d", c.name, method, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	dec = json.NewDecoder(bytes.NewReader(env.Result))
	dec.UseNumber()
	if err = dec.Decode(out); err != nil {
		return fmt.Errorf("%s RPC %s: unexpected result shape", c.name, method)
	}
	return nil
}

func (c *rpcClient) send(ctx context.Context, path string, body []byte) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s RPC: invalid request path", c.name)
	}
	r.Header.Set("Content-Type", "application/json")
	if c.hasAuth {
		if c.digest {
			if h := c.digestHeader(path); h != "" {
				r.Header.Set("Authorization", h)
			}
		} else {
			r.SetBasicAuth(c.user, c.pass)
		}
	}
	resp, err := c.http.Do(r)
	if err != nil {
		// Never surface the url.Error wrapper; the endpoint is not secret but keep messages short and uniform.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s RPC unreachable: timed out", c.name)
		}
		return nil, fmt.Errorf("%s RPC unreachable: %s", c.name, truncate(err.Error(), 160))
	}
	return resp, nil
}

// digestHeader answers the cached challenge (RFC 7616, MD5, qop=auth) or returns "" before the first 401.
func (c *rpcClient) digestHeader(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.challenge
	if ch == nil {
		return ""
	}
	c.nc++
	uri := path
	if u, err := url.Parse(c.endpoint + path); err == nil {
		uri = u.RequestURI()
	}
	nc := fmt.Sprintf("%08x", c.nc)
	cnonce := randomToken()[:16]
	ha1 := md5hex(c.user + ":" + ch["realm"] + ":" + c.pass)
	ha2 := md5hex(http.MethodPost + ":" + uri)
	var response, qop string
	if strings.Contains(","+strings.ReplaceAll(ch["qop"], " ", "")+",", ",auth,") {
		qop = "auth"
		response = md5hex(ha1 + ":" + ch["nonce"] + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = md5hex(ha1 + ":" + ch["nonce"] + ":" + ha2)
	}
	h := fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, algorithm=MD5, response=%q`, c.user, ch["realm"], ch["nonce"], uri, response)
	if qop != "" {
		h += fmt.Sprintf(`, qop=auth, nc=%s, cnonce=%q`, nc, cnonce)
	}
	if ch["opaque"] != "" {
		h += fmt.Sprintf(`, opaque=%q`, ch["opaque"])
	}
	return h
}

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// parseDigestChallenge picks the first Digest challenge whose algorithm is MD5 (or unspecified).
// monero-wallet-rpc offers both MD5-sess and MD5 challenges.
func parseDigestChallenge(headers []string) map[string]string {
	for _, h := range headers {
		if len(h) < 7 || !strings.EqualFold(h[:7], "Digest ") {
			continue
		}
		params := parseAuthParams(h[7:])
		if alg := strings.ToUpper(params["algorithm"]); alg != "" && alg != "MD5" {
			continue
		}
		if params["nonce"] == "" {
			continue
		}
		return params
	}
	return nil
}

func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimSpace(s[eq+1:])
		var val string
		if strings.HasPrefix(s, `"`) {
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			val = strings.ReplaceAll(s[1:min(end, len(s))], `\"`, `"`)
			s = s[min(end+1, len(s)):]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		out[key] = val
		s = strings.TrimPrefix(strings.TrimSpace(s), ",")
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
