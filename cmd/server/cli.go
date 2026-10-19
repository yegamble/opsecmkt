package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"opsecmkt/internal/brandingconfig"
	"opsecmkt/internal/dependencies"
	"opsecmkt/internal/emergency"
	"opsecmkt/internal/hostcontrol"
	"opsecmkt/internal/interactive"
	"opsecmkt/internal/keymaterial"
	"opsecmkt/internal/market"
	"opsecmkt/internal/operator"
	"opsecmkt/internal/ramsecrets"
)

const cliHelp = `usage: opsecmkt COMMAND [flags]

  setup             Guided setup, onion keys, or existing installation management
  serve             Run the application (all UI assets are embedded)
  upload-gateway    Receive, scan and normalize uploads on a separate host
  keys              Generate or import independent application/recovery keys
  versions-worker   Write a signed dependency update report outside the app
  secrets           Pack offline configuration, mount tmpfs and unlock from stdin
  emergency         Plan or explicitly target incident shutdown/LUKS erasure
  admin-tor-prepare Prepare public client authorization in the private Tor volume
  tor-identity-prepare Prepare a selected public onion identity in its Tor volume
  deploy            Manage a Docker Compose installation; deploy --help for commands
  host-control-agent Serve the private, fixed app-host reboot capability (Linux root)
  init|plan|install|redeploy|status|up|down|logs|doctor|storage
                   Short forms of deploy commands
  version           Print build information

Run serve --help, upload-gateway --help, keys --help or deploy --help for details.
Legacy server flags such as -preview, -bootstrap-admin and -status remain supported.
With no arguments in an interactive terminal, guided setup opens automatically.
`

// buildVersion and buildCommit are set at link time by release builds (-X main.buildVersion=<tag>
// -X main.buildCommit=<commit>); the admin dependency table shows both (A-553).
var buildVersion, buildCommit = "development", ""

// command handles operator commands before legacy server flags are parsed.
// serve sets the documented environment inputs and then uses the existing startup path.
func command(args []string, in io.Reader, out, errOut io.Writer) (bool, error) {
	if shouldStartInteractive(args, terminalPair(in, out)) {
		return true, runInteractive(in, out, errOut)
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") && args[0] != "--help" && args[0] != "-h" {
		return false, nil
	}
	switch args[0] {
	case "setup":
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			_, err := io.WriteString(out, "usage: opsecmkt setup\nGuided configuration, onion identity generation/import, or existing installation management.\nNo flags are required. Use numbered choices, ? for help, or quit to cancel.\nFlags on other commands remain available for automation.\n")
			return true, err
		}
		if len(args) != 1 {
			return true, errors.New("setup takes no flags; use setup --help")
		}
		return true, runInteractive(in, out, errOut)
	case "help", "--help", "-h":
		fmt.Fprint(out, cliHelp)
		return true, nil
	case "version":
		if len(args) != 1 {
			return true, errors.New("version takes no arguments")
		}
		version := buildVersion
		if b, ok := debug.ReadBuildInfo(); ok && version == "development" {
			for _, s := range b.Settings {
				if s.Key == "vcs.revision" {
					version = s.Value
				}
			}
		}
		fmt.Fprintln(out, "opsecmkt", version)
		return true, nil
	case "keys":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return true, keymaterial.RunContext(ctx, args[1:], out)
	case "admin-tor-prepare":
		return true, operator.PrepareAdminTor(args[1:], out)
	case "tor-identity-prepare":
		return true, operator.PrepareTorIdentity(args[1:], out)
	case "secrets", "emergency":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if args[0] == "secrets" {
			return true, ramsecrets.Run(ctx, args[1:], in, out, errOut)
		}
		return true, emergency.Run(ctx, args[1:], out, errOut)
	case "host-control-agent":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return true, hostcontrol.Run(ctx, args[1:], out, errOut, func(ctx context.Context, dir string) error {
			return operator.Run(ctx, []string{"storage", "check-bound", "--dir", dir}, nil, io.Discard, io.Discard)
		})
	case "deploy", "init", "plan", "install", "redeploy", "status", "up", "down", "logs", "doctor", "storage", "host-reboot":
		a := args
		if args[0] == "deploy" {
			a = args[1:]
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return true, operator.Run(ctx, a, in, out, errOut)
	case "upload-gateway":
		return true, runUploadGateway(args[1:], errOut)
	case "versions-worker":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return true, dependencies.Run(ctx, args[1:], out)
	case "serve":
		return false, configureServe(args[1:], errOut)
	default:
		return true, errors.New("unknown command; run opsecmkt --help")
	}
}

func shouldStartInteractive(args []string, terminal bool) bool {
	return terminal && (len(args) == 0 || len(args) == 1 && args[0] == "init" || len(args) == 2 && args[0] == "deploy" && args[1] == "init")
}

func terminalPair(in io.Reader, out io.Writer) bool {
	input, ok := in.(*os.File)
	if !ok {
		return false
	}
	output, ok := out.(*os.File)
	return ok && isInteractiveTerminal(input) && isInteractiveTerminal(output)
}

func runInteractive(in io.Reader, out, errOut io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Closing a terminal read makes Ctrl-C cancel a prompt immediately, even
	// when canonical input has not yet produced a complete line.
	if input, ok := in.(*os.File); ok {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				input.Close()
			case <-done:
			}
		}()
	}
	return interactive.Run(ctx, in, out, errOut)
}

func configureServe(args []string, errOut io.Writer) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	f.SetOutput(errOut)
	preview := f.Bool("preview", false, "read-only sample UI, no database")
	options := map[string]string{
		"branding-upload-origin": "BRANDING_UPLOAD_ORIGIN",
		"site-access":            "INITIAL_SITE_ACCESS", "registrations": "INITIAL_REGISTRATIONS",
		"addr": "ADDR", "app-mode": "APP_MODE", "database-url-file": "DATABASE_URL_FILE",
		"app-secret-key-file": "APP_SECRET_KEY_FILE", "setup-token-file": "SETUP_TOKEN_FILE",
		"monero-wallet-secret-file": "MONERO_WALLET_SECRET_FILE", "monero-wallet-rpc-url-file": "ESCROW_XMR_WALLET_RPC_URL_FILE",
		"monero-daemon-url-file": "ESCROW_XMR_DAEMON_URL_FILE", "monero-seed-key": "ESCROW_XMR_SEED_KEY",
		"monero-network": "MONERO_NETWORK", "escrow-xmr": "ESCROW_XMR", "clamd-address": "CLAMD_ADDRESS",
		"upload-mode": "UPLOAD_MODE", "upload-backend-addr": "UPLOAD_BACKEND_ADDR", "upload-ca": "UPLOAD_CA",
		"upload-cert": "UPLOAD_CERT", "upload-key": "UPLOAD_KEY", "tls-cert": "TLS_CERT", "tls-key": "TLS_KEY",
		"cookie-secure": "COOKIE_SECURE", "admin-socket": "ADMIN_SOCKET", "audit-signing-key-file": "AUDIT_SIGNING_KEY_FILE",
		"monero-rpc-ca": "MONERO_RPC_CA", "monero-rpc-cert": "MONERO_RPC_CERT", "monero-rpc-key": "MONERO_RPC_KEY",
		"profile": "OPSECMKT_PROFILE", "app-host": "OPSECMKT_APP_HOST", "gateway-url": "OPSECMKT_GATEWAY_URL",
		"dependency-report": "DEPENDENCY_REPORT_FILE", "dependency-report-public-key": "DEPENDENCY_REPORT_PUBLIC_KEY",
	}
	values := make(map[string]*string)
	for name, env := range options {
		help := "set " + env + " (existing environment is used when omitted)"
		if name == "branding-upload-origin" {
			help = "fixed public upload gateway origin: HTTPS or HTTP v3 onion; HTTP loopback is development-only (existing environment used when omitted)"
		} else if name == "site-access" {
			help = "initial visitor access: public or members (default public); saved administrator settings take precedence"
		} else if name == "registrations" {
			help = "initial new registrations: open or closed (default open); saved administrator settings take precedence"
		}
		values[name] = f.String(name, "", help)
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("serve takes flags only")
	}
	profile := os.Getenv("OPSECMKT_PROFILE")
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == "profile" {
			profile = *values[fl.Name]
		}
	})
	var invalidAccess error
	f.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "branding-upload-origin":
			value, err := brandingconfig.ParseOrigin(*values[fl.Name], profile == "" || profile == "dev")
			if err != nil {
				invalidAccess = err
			} else {
				*values[fl.Name] = value
			}
		case "site-access":
			if *values[fl.Name] != "public" && *values[fl.Name] != "members" {
				invalidAccess = errors.New("--site-access must be public or members")
			}
		case "registrations":
			if *values[fl.Name] != "open" && *values[fl.Name] != "closed" {
				invalidAccess = errors.New("--registrations must be open or closed")
			}
		}
	})
	if invalidAccess != nil {
		return invalidAccess
	}
	f.Visit(func(fl *flag.Flag) {
		if env := options[fl.Name]; env != "" {
			_ = os.Setenv(env, *values[fl.Name])
		}
	})
	os.Args = os.Args[:1]
	if *preview {
		os.Args = append(os.Args, "-preview")
		if v := os.Getenv("ADDR"); v != "" {
			_ = os.Setenv("PREVIEW_ADDR", v)
		}
	}
	return nil
}

func loadTLS(caFile, certFile, keyFile string, server bool) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("TLS certificate and key files are required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("cannot load TLS certificate/key pair")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	if caFile != "" {
		pool, err := readCAPool(caFile)
		if err != nil {
			return nil, err
		}
		c.RootCAs, c.ClientCAs = pool, pool
	}
	if server {
		if c.ClientCAs == nil {
			return nil, errors.New("upload backend requires a dedicated client CA")
		}
		c.ClientAuth = tls.RequireAndVerifyClientCert
	} else if c.RootCAs == nil {
		return nil, errors.New("upload gateway requires a backend CA")
	}
	return c, nil
}

func readCAPool(path string) (*x509.CertPool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read TLS CA file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read TLS CA file (maximum 1 MiB)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("TLS CA file contains no certificates")
	}
	return pool, nil
}

func runUploadGateway(args []string, errOut io.Writer) error {
	f := flag.NewFlagSet("upload-gateway", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", "127.0.0.1:8080", "HTTP ingress address behind Tor or an HTTPS reverse proxy")
	appURL := f.String("app-url", "", "upstream application origin (HTTPS for remote hosts)")
	appCA := f.String("app-ca", "", "optional application proxy CA PEM; default uses system trust (separate from upload backend CA)")
	backendURL := f.String("backend-url", "", "private HTTPS upload backend origin")
	ca := f.String("ca", "", "backend CA PEM")
	cert := f.String("cert", "", "gateway client certificate PEM")
	key := f.String("key", "", "gateway private key PEM")
	clamd := f.String("clamd-address", "127.0.0.1:3310", "local scanner address; never expose unauthenticated clamd to the internet")
	age := f.Int("max-signature-age-days", 30, "maximum scanner signature age, 7 through 90")
	privateHTTP := f.Bool("allow-private-http", false, "allow HTTP application proxy over an operator-managed private encrypted tunnel or local development network")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("upload-gateway takes flags only")
	}
	u, err := url.Parse(*appURL)
	if err != nil || u.Hostname() == "" {
		return errors.New("app-url must be an application origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !*privateHTTP && (ip == nil || !ip.IsLoopback()) {
		return errors.New("remote app-url requires HTTPS or --allow-private-http for an authenticated private tunnel")
	}
	tlsConfig, err := loadTLS(*ca, *cert, *key, false)
	if err != nil {
		return err
	}
	var appTLS *tls.Config
	if *appCA != "" {
		roots, err := readCAPool(*appCA)
		if err != nil {
			return err
		}
		appTLS = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := market.NewUploadGateway(ctx, market.UploadGatewayConfig{AppURL: *appURL, AppTLSConfig: appTLS, BackendURL: *backendURL, TLSConfig: tlsConfig, ClamdAddress: *clamd, MaxSignatureAgeDays: *age})
	if err != nil {
		return err
	}
	srv := newServer(*addr, h)
	// Authorized upload bodies may take 90s; the handler retains byte/concurrency limits.
	srv.ReadTimeout, srv.WriteTimeout = 90*time.Second, 150*time.Second
	ln, err := listen(srv)
	if err != nil {
		return err
	}
	return serve(ctx, srv, func() error { return srv.Serve(ln) }, 10*time.Second)
}

func privateUploadListener(app *market.App) (*http.Server, net.Listener, error) {
	addr := os.Getenv("UPLOAD_BACKEND_ADDR")
	if addr == "" {
		if os.Getenv("UPLOAD_MODE") == "isolated" {
			return nil, nil, errors.New("UPLOAD_MODE=isolated requires UPLOAD_BACKEND_ADDR and mutual TLS files")
		}
		return nil, nil, nil
	}
	if os.Getenv("UPLOAD_MODE") != "isolated" {
		return nil, nil, errors.New("UPLOAD_BACKEND_ADDR requires UPLOAD_MODE=isolated")
	}
	c, err := loadTLS(os.Getenv("UPLOAD_CA"), os.Getenv("UPLOAD_CERT"), os.Getenv("UPLOAD_KEY"), true)
	if err != nil {
		return nil, nil, err
	}
	srv := newServer(addr, app.UploadBackendHandler())
	ln, err := listen(srv)
	if err != nil {
		return nil, nil, err
	}
	return srv, tls.NewListener(ln, c), nil
}

func checkListenerSettings() error {
	isolated := os.Getenv("UPLOAD_MODE") == "isolated"
	addr := os.Getenv("UPLOAD_BACKEND_ADDR")
	if isolated != (addr != "") {
		return errors.New("isolated uploads require UPLOAD_MODE=isolated and UPLOAD_BACKEND_ADDR together")
	}
	if addr != "" {
		if _, err := loadTLS(os.Getenv("UPLOAD_CA"), os.Getenv("UPLOAD_CERT"), os.Getenv("UPLOAD_KEY"), true); err != nil {
			return err
		}
	} else if os.Getenv("UPLOAD_CA") != "" || os.Getenv("UPLOAD_CERT") != "" || os.Getenv("UPLOAD_KEY") != "" {
		return errors.New("upload TLS files require an isolated upload listener")
	}
	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	if cert != "" || key != "" {
		if cert == "" || key == "" {
			return errors.New("TLS_CERT and TLS_KEY must be set together")
		}
		if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
			return errors.New("cannot load application TLS certificate/key pair")
		}
	}
	return nil
}

func publicTLSListener(ln net.Listener) (net.Listener, error) {
	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	if cert == "" && key == "" {
		return ln, nil
	}
	if cert == "" || key == "" {
		return nil, errors.New("TLS_CERT and TLS_KEY must be set together")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("cannot load application TLS certificate/key pair")
	}
	return tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}), nil
}
