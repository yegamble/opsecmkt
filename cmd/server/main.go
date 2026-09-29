package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"opsecmkt/internal/market"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// maxConns caps the server's simultaneous connections (A-178). The per-class slots in internal/market bound the
// work; this cap only bounds goroutines and buffers. It is high enough that trickled headers (each connection
// lasts at most ReadHeaderTimeout, 5 s) need about 400 new connections a second to hold it, and low enough that
// every connection reading a 64 KiB form body at once needs 128 MiB, well inside the app's GOMEMLIMIT.
//
// Memory budget with photo uploads (A-186): those 128 MiB of form bodies, plus 2 upload bodies of at most 4 MiB
// (uploadWork admits two; every other upload, and every refused one, is read into io.Discard, not memory), plus
// one decode at a time (imageDecodeWork), refused before it starts unless its estimate is at most 64 MiB (an
// upper bound on what image/jpeg and image/png allocate, plus up to 256 KiB of their fixed tables and buffers:
// TestDecodeAllocationWithinEstimate, FuzzDecodeReencode), plus its outputs (a resampled copy of at most a quarter
// of the decoded pixels, the 1200 px and thumbnail images and their JPEGs, about 20 MiB): about 220 MiB live
// (TestUploadConcurrentMaxSizeStaysInBudget). Serving images (A-187) adds at most 96 MiB: the bytes that image
// responses hold from their query until written, reserved from market's imageBytes (64 MiB, /img) and
// deskImageBytes (32 MiB, /desk/img) budgets or refused with 503 (TestImageSlowReadersBoundedByByteBudget). About
// 320 MiB in all, plus the database driver's buffers for rows being read, inside the 400 MiB GOMEMLIMIT.
const maxConns = market.MaxConns

// newServer is the HTTP server with its read and write bounds.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
}

// listen opens srv's address with at most maxConns open connections (A-178); later clients wait in the kernel's
// accept backlog. While every slot is taken, keep-alive is off: idle connections are closed and each response
// ends its own.
func listen(srv *http.Server) (net.Listener, error) {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return nil, err
	}
	return market.LimitListener(ln, maxConns, func(full bool) { srv.SetKeepAlivesEnabled(!full) }), nil
}

func main() {
	healthcheckFlag := flag.Bool("healthcheck", false, "probe the local server's /healthz (ADDR's port, default 8080) and exit 0 only when it answers ok; prints nothing on success (the Compose healthcheck)")
	statusFlag := flag.Bool("status", false, "print installed=true|false schema=NNN keyroot=app_secret_key|setup_token for DATABASE_URL and exit (read-only; starts no server; prints no secret)")
	preview := flag.Bool("preview", false, "read-only UI preview with sample data, binds loopback only")
	resetAdmin := flag.String("reset-admin-password", "", "host-only break-glass: read a new password for this administrator handle from stdin, end its sessions and exit (starts no server)")
	bootstrap := flag.String("bootstrap-admin", "", "host-only first-run setup: create the first administrator with this handle, reading its password from stdin, mark the market installed and exit (starts no server; no setup token needed)")
	siteName := flag.String("site-name", "", "with -bootstrap-admin: the marketplace name (default OPSMKT, at most 80 bytes)")
	setRoleFlag := flag.String("set-role", "", "host-only: -set-role HANDLE ROLE changes that account's role to buyer, vendor, moderator or admin (the only way to add or remove an administrator; removing one ends its sessions) and exits (starts no server)")
	resetFactors := flag.String("reset-admin-factors", "", "host-only break-glass: turn off this administrator's TOTP, recovery codes and PGP sign-in, end its sessions and exit (reads no stdin; starts no server)")
	flag.Parse()
	if *healthcheckFlag {
		if err := healthcheck(os.Getenv("ADDR"), 4*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
	if *statusFlag {
		if err := status(context.Background(), os.Getenv("DATABASE_URL"), os.Getenv, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			os.Exit(1)
		}
		return
	}
	if flagGiven("reset-admin-factors") {
		if err := resetAdminFactors(context.Background(), *resetFactors, flagGiven, flag.Args(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "reset-admin-factors:", err)
			os.Exit(1)
		}
		return
	}
	if flagGiven("set-role") {
		if err := setRole(context.Background(), *setRoleFlag, flagGiven, flag.Args(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "set-role:", err)
			os.Exit(1)
		}
		return
	}
	if flagGiven("bootstrap-admin") || flagGiven("site-name") {
		if err := bootstrapAdmin(context.Background(), *bootstrap, *siteName, flagGiven, flag.Args(), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap-admin:", err)
			os.Exit(1)
		}
		return
	}
	reset := false
	flag.Visit(func(f *flag.Flag) { reset = reset || f.Name == "reset-admin-password" })
	if reset {
		if err := resetAdminPassword(context.Background(), *resetAdmin, *preview, flag.Args(), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "reset-admin-password:", err)
			os.Exit(1)
		}
		return
	}
	app, err := market.New(context.Background(), *preview)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if *preview {
		addr = os.Getenv("PREVIEW_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		host, _, parseErr := net.SplitHostPort(addr)
		ip := net.ParseIP(host)
		if parseErr != nil || ip == nil || !ip.IsLoopback() {
			app.Close()
			log.Fatal("PREVIEW_ADDR must be a loopback IP address and port")
		}
	}
	srv := newServer(addr, app)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Start(ctx)
	ln, err := listen(srv)
	if err != nil {
		app.Close()
		log.Fatal(err)
	}
	log.Printf("OPSMKT listening on %s (preview=%v)", addr, *preview)
	err = serve(ctx, srv, func() error { return srv.Serve(ln) }, 10*time.Second)
	// Only after in-flight requests have finished: stop the payment watcher (Close waits for an in-flight
	// payout, which runs on its own bounded context) and close the database.
	app.Close()
	if err != nil {
		log.Fatal(err)
	}
}

// serve runs listen until ctx is cancelled, then shuts srv down gracefully. It returns only after Shutdown
// has finished (in-flight requests completed or grace elapsed), so the caller can then release what the
// handlers use. A listen error other than http.ErrServerClosed is returned at once.
func serve(ctx context.Context, srv *http.Server, listen func() error, grace time.Duration) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(c); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	if err := listen(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}
