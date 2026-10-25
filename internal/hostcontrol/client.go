package hostcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Client reaches one root-owned Unix socket. It cannot select another host.
type Client struct {
	SocketPath string
	transport  http.RoundTripper
}

func NewClient(socket string) *Client { return &Client{SocketPath: socket} }

func (c *Client) Status(ctx context.Context) (Status, error) {
	var result Status
	err := c.call(ctx, http.MethodGet, "/v1/status", nil, &result, 10*time.Second)
	return result, err
}

func (c *Client) RequestReboot(ctx context.Context, request Request) (Receipt, error) {
	var result Receipt
	data, err := json.Marshal(request)
	if err != nil {
		return result, errors.New("host control request unavailable")
	}
	err = c.call(ctx, http.MethodPost, "/v1/reboot", data, &result, 15*time.Second)
	if err == nil && (result.RequestID != request.RequestID || !validRequestID(result.RequestID) || !validState(result.State) || result.AcceptedAt.IsZero() || !result.RebootAt.Equal(result.AcceptedAt.Add(rebootGrace))) {
		err = errors.New("host control returned an invalid receipt")
	}
	return result, err
}

func (c *Client) call(ctx context.Context, method, path string, data []byte, result any, timeout time.Duration) error {
	unavailable := errors.New("host control is unavailable; the request outcome must be checked before another request")
	if c == nil || !safeAbsolute(c.SocketPath) {
		return unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", c.SocketPath)
		if err != nil {
			return nil, unavailable
		}
		if err := verifyRootPeer(conn); err != nil {
			conn.Close()
			return nil, unavailable
		}
		return conn, nil
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return unavailable
	}
	req.Header.Set("Content-Type", "application/json")
	var roundTrip http.RoundTripper = transport
	if c.transport != nil {
		roundTrip = c.transport
	}
	response, err := (&http.Client{Transport: roundTrip, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return unavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && !(method == http.MethodPost && response.StatusCode == http.StatusAccepted) {
		return unavailable
	}
	responseData, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(responseData) > 4096 {
		return unavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(responseData))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil || decoder.Decode(new(any)) != io.EOF {
		return unavailable
	}
	return nil
}
