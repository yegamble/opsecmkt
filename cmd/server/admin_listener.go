package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"opsecmkt/internal/market"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Keep private connections apart from public capacity. This adds at most 4 MiB of bounded form bodies to
// main.go's memory budget, and a public connection flood cannot take the administrator's connection slots.
const maxAdminConns = 64

func listenAdmin(srv *http.Server) (net.Listener, error) {
	dir, err := os.Lstat(filepath.Dir(srv.Addr))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0007 != 0 || dir.Mode().Perm()&0020 != 0 {
		return nil, errors.New("ADMIN_SOCKET directory must exist, be a real directory, and allow no access to others or group writes")
	}
	if entry, err := os.Lstat(srv.Addr); err == nil {
		if entry.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("ADMIN_SOCKET already exists and is not a socket; refusing to remove it")
		}
		conn, err := net.DialTimeout("unix", srv.Addr, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil, errors.New("ADMIN_SOCKET is already in use")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errors.New("ADMIN_SOCKET cannot be checked safely; refusing to remove it")
		}
		if err := os.Remove(srv.Addr); err != nil {
			return nil, errors.New("ADMIN_SOCKET stale socket could not be removed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("ADMIN_SOCKET could not be inspected")
	}
	ln, err := net.Listen("unix", srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("ADMIN_SOCKET listen failed: %w", err)
	}
	if err := os.Chmod(srv.Addr, 0660); err != nil {
		ln.Close()
		return nil, errors.New("ADMIN_SOCKET permissions could not be restricted")
	}
	return market.LimitListener(ln, maxAdminConns, func(full bool) { srv.SetKeepAlivesEnabled(!full) }), nil
}

// serveAll stops every listener when one fails and waits for every graceful shutdown before the database
// and background work can be closed. All listeners are opened before any serves a request.
func serveAll(ctx context.Context, servers []*http.Server, listeners []net.Listener, grace time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			results <- serve(ctx, srv, func() error {
				err := srv.Serve(listeners[i])
				cancel() // stop sibling listeners promptly, while serve waits for this server's shutdown too
				return err
			}, grace)
		}()
	}
	var first error
	for range servers {
		if err := <-results; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}
