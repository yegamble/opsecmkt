package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthcheckURL is the /healthz of a server listening on addr (ADDR; blank is the default ":8080"): the same
// port on 127.0.0.1 when addr binds every interface, else addr's own host.
func healthcheckURL(addr string) (string, error) {
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", errors.New("ADDR must be host:port or :port")
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// healthcheck is -healthcheck, the Compose app healthcheck: it succeeds only when /healthz answers 200 with
// the body "ok". The caller prints nothing on success; errors name a class only (the status code, never a
// response body).
func healthcheck(addr string, timeout time.Duration) error {
	u, err := healthcheckURL(addr)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(u)
	if err != nil {
		return errors.New("no answer from the server on " + u)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16))
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "ok" {
		return fmt.Errorf("unhealthy: %s answered HTTP %d", u, resp.StatusCode)
	}
	return nil
}
