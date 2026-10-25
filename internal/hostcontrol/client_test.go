package hostcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeClient(code int, body string) *Client {
	return &Client{SocketPath: "/run/test/control.sock", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestClientProtocolUsesOnlyFixedLocalPathsAndFields(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	request := Request{Version: 1, Action: RebootAction, RequestID: testID, RequestedAt: now}
	receipt := Receipt{RequestID: testID, State: "accepted", AcceptedAt: now, RebootAt: now.Add(30 * time.Second)}
	data, _ := json.Marshal(receipt)
	client := fakeClient(http.StatusAccepted, string(data))
	client.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Host != "localhost" || r.URL.Path != "/v1/reboot" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("client selected unexpected host, command or transport")
		}
		var sent Request
		if json.NewDecoder(r.Body).Decode(&sent) != nil || sent != request {
			t.Fatal("client changed request identity")
		}
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
	})
	result, err := client.RequestReboot(context.Background(), request)
	if err != nil || result != receipt {
		t.Fatalf("client did not return receipt: %#v %v", result, err)
	}
	status, err := fakeClient(http.StatusOK, `{"ready":false,"reason":"unlock-not-rehearsed"}`).Status(context.Background())
	if err != nil || status.Ready || status.Reason != "unlock-not-rehearsed" {
		t.Fatal("client status unavailable")
	}
}

func TestClientRejectsMalformedMismatchedOversizedAndErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{http.StatusConflict, `{"error":"private-path-must-not-echo"}`},
		{http.StatusOK, `{"ready":true,"reason":"ready","unknown":true}`},
		{http.StatusOK, `{"ready":true,"reason":"ready"} {}`},
		{http.StatusOK, `{"ready":true,"reason":"ready"}` + strings.Repeat(" ", 4096)},
		{http.StatusOK, `malformed-private-path-must-not-echo`},
	} {
		if _, err := fakeClient(tc.code, tc.body).Status(context.Background()); err == nil || strings.Contains(err.Error(), "private-path") {
			t.Fatal("unsafe response accepted or diagnostic leaked")
		}
	}
	client := fakeClient(http.StatusOK, "")
	client.transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private-path-must-not-echo") })
	if _, err := client.Status(context.Background()); err == nil || strings.Contains(err.Error(), "private-path") {
		t.Fatal("transport failure diagnostic leaked")
	}
	request := Request{RequestID: testID}
	if _, err := fakeClient(http.StatusAccepted, `{"request_id":"wrong","state":"accepted"}`).RequestReboot(context.Background(), request); err == nil {
		t.Fatal("mismatched receipt accepted")
	}
}

func TestClientInvalidSocketCannotDial(t *testing.T) {
	for _, path := range []string{"", "relative", "/run/../tmp/control.sock"} {
		client := NewClient(path)
		if _, err := client.Status(context.Background()); err == nil {
			t.Fatal("invalid socket path accepted")
		}
	}
}
