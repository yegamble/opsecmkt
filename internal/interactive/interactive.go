// Package interactive presents explicit operator/key commands as a line-based
// setup conversation. The underlying packages remain the validation authority.
package interactive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"opsecmkt/internal/keymaterial"
	"opsecmkt/internal/onionidentity"
	"opsecmkt/internal/operator"
)

var ErrCancelled = errors.New("interactive setup cancelled")

type runners struct {
	deploy func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
	keys   func(context.Context, []string, io.Writer) error
}

type wizard struct {
	ctx         context.Context
	in          io.Reader
	out, errOut io.Writer
	run         runners
	answers     map[string]string
	lines       chan inputLine
}
type inputLine struct {
	text string
	err  error
}

// Run is explicitly selected by the dispatcher, which owns TTY detection.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	return run(ctx, in, out, errOut, runners{operator.Run, keymaterial.RunContext})
}

func run(ctx context.Context, in io.Reader, out, errOut io.Writer, execute runners) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &wizard{ctx: ctx, in: in, out: out, errOut: errOut, run: execute, answers: map[string]string{}, lines: make(chan inputLine)}
	// Only this goroutine reads input. Cancellation never waits for a blocked
	// terminal read and cannot proceed to a mutation after a cancelled prompt.
	go func() {
		s := bufio.NewScanner(in)
		s.Buffer(make([]byte, 4096), 64<<10)
		for s.Scan() {
			select {
			case w.lines <- inputLine{text: s.Text()}:
			case <-ctx.Done():
				return
			}
		}
		err := s.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case w.lines <- inputLine{err: err}:
		case <-ctx.Done():
		}
	}()
	fmt.Fprintln(out, "OPSECMKT setup\nEnter ? for help, quit to cancel, or press Ctrl-C. Enter accepts the displayed default; :clear removes an optional cached answer.\nSupply private file paths, never passwords or credential URLs. Nothing starts without a separate confirmation.")
	action, err := w.ask("task", "Task: 1 new deployment, 2 onion identity, 3 manage existing installation", "1", "Use manage for an existing market. New initialization never adopts or replaces its keys.", oneOf("1", "2", "3"))
	if err != nil {
		return err
	}
	switch action {
	case "1":
		return w.create()
	case "2":
		return w.onion()
	default:
		return w.manage()
	}
}

func (w *wizard) ask(key, label, fallback, help string, validate func(string) error) (string, error) {
	if prior, ok := w.answers[key]; ok {
		fallback = prior
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return "", err
		}
		shown := fallback
		if shown == "" {
			shown = "blank"
		}
		fmt.Fprintf(w.out, "%s [%s]: ", label, shown)
		var line inputLine
		select {
		case <-w.ctx.Done():
			return "", w.ctx.Err()
		case line = <-w.lines:
		}
		if line.err != nil {
			if errors.Is(line.err, io.EOF) {
				return "", ErrCancelled
			}
			return "", errors.New("cannot read setup input")
		}
		if len(line.text) > 4096 || strings.IndexFunc(line.text, unicode.IsControl) >= 0 {
			fmt.Fprintln(w.errOut, "Use one line of at most 4096 characters, without control characters.")
			continue
		}
		value := strings.TrimSpace(line.text)
		if value == "?" {
			fmt.Fprintln(w.out, help)
			continue
		}
		if strings.EqualFold(value, "quit") || strings.EqualFold(value, "q") {
			return "", ErrCancelled
		}
		if value == ":clear" {
			if validate != nil && validate("") != nil {
				fmt.Fprintln(w.errOut, "This answer is required and cannot be cleared.")
				continue
			}
			value = ""
		} else if value == "" {
			value = fallback
		}
		if validate != nil {
			if err := validate(value); err != nil {
				fmt.Fprintln(w.errOut, err)
				continue
			}
		}
		w.answers[key] = value
		return value, nil
	}
}

func oneOf(choices ...string) func(string) error {
	return func(v string) error {
		for _, c := range choices {
			if v == c {
				return nil
			}
		}
		return errors.New("Choose one of the displayed values.")
	}
}
func required(v string) error {
	if v == "" {
		return errors.New("A value is required.")
	}
	return nil
}
func optional(check func(string) error) func(string) error {
	return func(v string) error {
		if v == "" {
			return nil
		}
		return check(v)
	}
}

var digest = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var contextName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,48}$`)

func image(v string) error {
	if !digest.MatchString(v) {
		return errors.New("Use a reviewed repository image@sha256: digest with 64 lowercase hex characters.")
	}
	return nil
}
func dockerContext(v string) error {
	if !contextName.MatchString(v) {
		return errors.New("Use a Docker context name with lowercase letters, digits, underscores or hyphens (at most 49 characters).")
	}
	return nil
}
func newDirectory(v string) error {
	if required(v) != nil {
		return required(v)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return errors.New("Invalid directory path.")
	}
	if _, err := os.Lstat(abs); !os.IsNotExist(err) {
		return errors.New("Choose a new directory; existing paths are never replaced.")
	}
	return directory(filepath.Dir(abs))
}
func directory(v string) error {
	info, err := os.Lstat(v)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Use an existing real directory, not a symlink.")
	}
	return nil
}
func absolute(v string) error {
	if !filepath.IsAbs(v) || filepath.Clean(v) != v || v == "/" || strings.Contains(v, "$") {
		return errors.New("Use a clean absolute directory path other than /.")
	}
	return nil
}
func file(private bool) func(string) error {
	return func(v string) error {
		info, err := os.Lstat(v)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Size() == 0 || private && info.Mode().Perm()&0077 != 0 {
			return errors.New("Use a nonempty regular file of at most 64 KiB; private inputs require mode 0600 or stricter, with no symlink.")
		}
		return nil
	}
}
func bind(v string) error {
	host, port, err := net.SplitHostPort(v)
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 || ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) {
		return errors.New("Use an explicit loopback/private IP and port, such as 127.0.0.1:8080.")
	}
	return nil
}
func origin(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || !(u.Scheme == "https" || u.Scheme == "http" && strings.HasSuffix(u.Hostname(), ".onion")) {
		return errors.New("Use a public HTTPS origin or HTTP .onion origin, without credentials, path, query or fragment.")
	}
	return nil
}
func host(v string) error {
	if v == "" || strings.ContainsAny(v, " /:@?#") {
		return errors.New("Use the private application DNS name or IP, without scheme or port.")
	}
	return nil
}
func prefix(v string) error {
	if len(v) > 6 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		return errors.New("Use at most 6 lowercase a-z or 2-7 characters, or leave blank for random.")
	}
	return nil
}

func (w *wizard) flag(args *[]string, key, label, fallback, help string, check func(string) error) (string, error) {
	v, err := w.ask(key, label, fallback, help, check)
	if err == nil && v != "" {
		*args = append(*args, "--"+key, v)
	}
	return v, err
}
func (w *wizard) yes(key, label, help string) (bool, error) {
	v, err := w.ask(key, label, "n", help, oneOf("y", "n"))
	return v == "y", err
}

func preview(out io.Writer, prefix string, args []string) {
	fmt.Fprintln(out, "\nReview equivalent command (paths only; no secret contents):")
	fmt.Fprint(out, prefix)
	for _, arg := range args {
		fmt.Fprint(out, " ", shellQuote(arg))
	}
	fmt.Fprintln(out)
}
func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }

func (w *wizard) searchNotice(args []string) {
	for i, arg := range args {
		if (arg == "--prefix" || strings.HasSuffix(arg, "onion-prefix")) && i+1 < len(args) && args[i+1] != "" {
			fmt.Fprintln(w.out, "Searching locally for a matching prefix; time is unpredictable and Ctrl-C cancels. Configuration/identity files are saved only after success; no onion service is published.")
			return
		}
	}
}

func (w *wizard) create() error {
	for {
		args, dir, err := w.collect()
		if err != nil {
			return err
		}
		preview(w.out, "opsecmkt deploy", args)
		fmt.Fprintln(w.out, "Blank key-import paths generate independent app/setup/audit secrets; escrow also generates a wallet secret unless imported. Existing state requires its original stable keys. Tor role identities are generated or imported as selected.\nThis creates private configuration only. Remote dependency hosts are prepared separately.")
		answer, err := w.ask("review-create", "Create configuration? y=yes, n=cancel, edit=change answers", "n", "Edit reopens prompts with previous answers as defaults. No writes occur until y.", oneOf("y", "n", "edit"))
		if err != nil {
			return err
		}
		delete(w.answers, "review-create")
		if answer == "edit" {
			continue
		}
		if answer != "y" {
			fmt.Fprintln(w.out, "Cancelled; no configuration created.")
			return nil
		}
		if err := w.ctx.Err(); err != nil {
			return err
		}
		w.searchNotice(args)
		if err := w.run.deploy(w.ctx, args, w.in, w.out, w.errOut); err != nil {
			return err
		}
		w.materialSummary(args, dir)
		if err := w.run.deploy(w.ctx, []string{"plan", "--dir", dir}, w.in, w.out, w.errOut); err != nil {
			return err
		}
		fmt.Fprintf(w.out, "\nConfiguration retained in %s. Back up operator.json, secrets/, any onion-public/ or onion-admin/ identities, public admin-clients/, and external retention state through the documented encrypted recovery procedure. Keep PGP recovery/CA signing keys offline. Tor runtime volumes remain persistent.\nNext: opsecmkt deploy install --dir %s\n", dir, shellQuote(dir))
		start, err := w.yes("start", "Start this installation now? y/n", "Default n leaves configuration ready for review. y calls deploy install and may contact Docker/dependencies.")
		if err != nil {
			return err
		}
		if start {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			return w.run.deploy(w.ctx, []string{"install", "--dir", dir}, w.in, w.out, w.errOut)
		}
		return nil
	}
}

func valueOf(args []string, flag string) string {
	for i, arg := range args {
		if arg == "--"+flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (w *wizard) materialSummary(args []string, dir string) {
	fmt.Fprintln(w.out, "\nSaved key material (contents are never displayed):")
	for _, item := range []struct{ flag, name, label string }{
		{"app-secret-key-file", "app-secret-key", "application key"}, {"setup-token-file", "setup-token", "setup token"}, {"audit-signing-key-file", "audit-signing-key", "audit signing seed"},
	} {
		status := "Generated"
		if valueOf(args, item.flag) != "" {
			status = "Imported"
		}
		fmt.Fprintf(w.out, "%s %s: %s\n", status, item.label, filepath.Join(dir, "secrets", item.name))
	}
	if valueOf(args, "database-url-file") == "" {
		fmt.Fprintf(w.out, "Generated development database credentials: %s and %s\n", filepath.Join(dir, "secrets", "database-url"), filepath.Join(dir, "secrets", "postgres-password"))
	} else {
		fmt.Fprintf(w.out, "Imported database URL: %s\n", filepath.Join(dir, "secrets", "database-url"))
	}
	for _, arg := range args {
		if arg == "--escrow-xmr" {
			status := "Generated"
			if valueOf(args, "monero-wallet-secret-file") != "" {
				status = "Imported"
			}
			fmt.Fprintf(w.out, "%s stable Monero wallet secret: %s\nImported RPC URL files: %s and %s\nImported public recovery recipient: %s\n", status, filepath.Join(dir, "secrets", "monero-wallet-secret"), filepath.Join(dir, "secrets", "escrow-xmr-wallet-rpc-url"), filepath.Join(dir, "secrets", "escrow-xmr-daemon-url"), filepath.Join(dir, "secrets", "escrow-xmr-seed-key.asc"))
		}
	}
	for _, role := range []struct{ image, prefix, path, label string }{{"tor-image", "", "onion-public", "public"}, {"admin-tor-image", "admin-", "onion-admin", "administrator"}} {
		if valueOf(args, role.image) == "" {
			continue
		}
		status := "Generated"
		if valueOf(args, role.prefix+"onion-address") != "" {
			status = "Imported"
		}
		fmt.Fprintf(w.out, "%s %s onion identity: %s (hostname and both Tor key files)\n", status, role.label, filepath.Join(dir, role.path))
	}
	if valueOf(args, "admin-tor-image") != "" {
		fmt.Fprintf(w.out, "Imported public client allowlist: %s\nAfter install, open /setup on the client-authorized administrator onion shown by plan. The public storefront refuses setup.\n", filepath.Join(dir, "admin-clients"))
	} else if valueOf(args, "exposure") == "tor" {
		fmt.Fprintln(w.out, "After install, open /setup on the development public onion shown by plan.")
	} else {
		origin := valueOf(args, "gateway-url")
		if origin == "" {
			origin = "http://" + valueOf(args, "app-bind")
		}
		fmt.Fprintf(w.out, "After install, open %s/setup and use the saved private setup-token file locally.\n", strings.TrimSuffix(origin, "/"))
	}
}

func (w *wizard) collect() ([]string, string, error) {
	fmt.Fprintln(w.out, "\n1. Deployment and host boundaries\nFor an existing marketplace, cancel and manage its retained installation; do not create replacement custody keys.")
	args := []string{"init"}
	mode, err := w.flag(&args, "mode", "Profile: dev or prod", "dev", "dev permits one host and stagenet. prod requires prepared separate database, payment and upload/scanner hosts.", oneOf("dev", "prod"))
	if err != nil {
		return nil, "", err
	}
	if mode == "prod" {
		fmt.Fprintln(w.out, "Production storage comes first: run on the Linux Docker host after installing an encrypted LUKS2 root. Initialization verifies root, Docker/containerd state, retention and private inputs before creating keys. A new data volume cannot encrypt an existing root; use opsecmkt deploy storage --help for additional disks. Plain or unverifiable storage is refused.")
	}
	dir, err := w.flag(&args, "dir", "New private installation directory", "./opsecmkt-deployment", "The parent must exist. init refuses an existing directory and preserves all existing installations.", newDirectory)
	if err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "docker-context", "Explicit Docker context", "default", "Inspect its endpoint separately; a named context can target another host. Ambient DOCKER_HOST/DOCKER_CONTEXT are ignored.", dockerContext); err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "app-image", "Reviewed application image digest", "", "Use the repository app image contract, image@sha256:DIGEST. No latest tag or automatic build.", image); err != nil {
		return nil, "", err
	}
	exposure, err := w.flag(&args, "exposure", "Exposure: clearnet or tor", "clearnet", "Development Tor can run locally. Production public Tor belongs to the remote gateway; private administrator Tor runs on the app host.", oneOf("clearnet", "tor"))
	if err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "app-bind", "Application private bind", "127.0.0.1:8080", "Remote gateway needs a verified private route or independently authenticated encrypted tunnel to this listener.", bind); err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "retention-dir", "Persistent absolute retention directory (blank uses install/retention)", "", "RAM-unlocked installations require external persistent retention storage. This path is not a backup.", optional(absolute)); err != nil {
		return nil, "", err
	}

	if _, err = w.flag(&args, "site-access", "Initial visitor access: public or members", "public", "public permits signed-out browsing; members requires an account to view the site and listings. This seeds the saved administrator setting only on first start.", oneOf("public", "members")); err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "registrations", "Initial new registrations: open or closed", "open", "closed refuses new account creation; existing accounts can still sign in. The administrator can change this saved setting later.", oneOf("open", "closed")); err != nil {
		return nil, "", err
	}

	fmt.Fprintln(w.out, "\n2. Database, TLS and upload boundary")
	dbCheck := optional(file(true))
	if mode == "prod" {
		dbCheck = file(true)
	}
	if _, err = w.flag(&args, "database-url-file", "PostgreSQL URL private file (blank creates dev DB)", "", "File contains the credential URL; never type it here. Production needs a separate host with sslmode=verify-full.", dbCheck); err != nil {
		return nil, "", err
	}
	if _, err = w.flag(&args, "database-ca", "PostgreSQL public CA file (blank uses system trust)", "", "Public PEM CA only; its signing key stays offline.", optional(file(false))); err != nil {
		return nil, "", err
	}
	if mode == "prod" {
		fmt.Fprintln(w.out, "Prepare the remote gateway, ClamAV, public ingress, firewall and supervisor separately. This wizard manages the app host.")
		for _, f := range []struct {
			key, label, def, help string
			check                 func(string) error
		}{
			{"app-host", "Private application hostname/IP", "", "Must match app TLS and remain distinct from dependency/gateway hosts.", host},
			{"gateway-url", "Public gateway origin", "", "HTTPS origin or HTTP .onion origin on the separate upload host.", origin},
			{"backend-bind", "Private mTLS upload-backend bind", "127.0.0.1:8443", "Permit only the gateway over its private authenticated route.", bind},
			{"tls-cert", "Application HTTPS server certificate file", "", "Leaf certificate with the application hostname SAN.", file(false)},
			{"tls-key", "Application HTTPS private key file", "", "Private 0600 leaf key; do not supply a CA signing key.", file(true)},
			{"upload-ca", "Upload client-auth CA public certificate file", "", "Dedicated CA authenticating the separate gateway client.", file(false)},
			{"upload-cert", "Upload-backend server certificate file", "", "Backend server identity; gateway verifies its hostname.", file(false)},
			{"upload-key", "Upload-backend private key file", "", "Private 0600 backend leaf key.", file(true)},
		} {
			if _, err = w.flag(&args, f.key, f.label, f.def, f.help, f.check); err != nil {
				return nil, "", err
			}
		}
	} else {
		if _, err = w.flag(&args, "clamav-image", "Optional development ClamAV image digest", "", "Leave blank to disable uploads. A separate container on this host is development-only; include fresh signatures.", optional(image)); err != nil {
			return nil, "", err
		}
	}

	fmt.Fprintln(w.out, "\n3. Stable keys and optional Monero escrow")
	for _, f := range []struct{ key, label string }{{"app-secret-key-file", "Existing app secret file (blank generates for NEW state)"}, {"setup-token-file", "Existing setup-token file (blank generates)"}, {"audit-signing-key-file", "Existing audit seed file (blank generates)"}} {
		if _, err = w.flag(&args, f.key, f.label, "", "Import the stable private 0600 file when retaining established data. Do not paste contents.", optional(file(true))); err != nil {
			return nil, "", err
		}
	}
	escrow, err := w.yes("escrow", "Enable custodial Monero escrow? y/n", "Default n moves no funds. Reuse only this installation's dedicated wallet, network and stable wallet secret.")
	if err != nil {
		return nil, "", err
	}
	if escrow {
		args = append(args, "--escrow-xmr")
		if mode == "prod" {
			if _, err = w.flag(&args, "monero-network", "Monero network: stagenet or mainnet", "stagenet", "Mainnet moves real funds and has no recorded deployment verification. Stagenet is for rehearsal.", oneOf("stagenet", "mainnet")); err != nil {
				return nil, "", err
			}
		}
		for _, f := range []struct {
			key, label        string
			private, optional bool
		}{
			{"monero-wallet-rpc-url-file", "Dedicated wallet RPC URL private file", true, false},
			{"monero-daemon-url-file", "Restricted daemon RPC URL private file", true, false},
			{"monero-wallet-secret-file", "Stable wallet secret file (blank only for NEW wallets)", true, true},
			{"monero-seed-key", "OpenPGP PUBLIC escrow recovery key file", false, false},
			{"monero-rpc-ca", "Optional Monero public TLS CA file", false, true},
			{"monero-rpc-cert", "Optional Monero mTLS client certificate file", false, true},
		} {
			check := file(f.private)
			if f.optional {
				check = optional(check)
			}
			if _, err = w.flag(&args, f.key, f.label, "", "Production RPC needs separate verified HTTPS and authentication. Keep recovery private keys and CA signing keys offline; preserve the established wallet secret.", check); err != nil {
				return nil, "", err
			}
		}
		if w.answers["monero-rpc-cert"] != "" {
			if _, err = w.flag(&args, "monero-rpc-key", "Monero mTLS private client key file", "", "Required with the selected client certificate.", file(true)); err != nil {
				return nil, "", err
			}
		}
	}

	fmt.Fprintln(w.out, "\n4. Tor service identities")
	if exposure == "tor" {
		if mode == "dev" {
			if _, err = w.flag(&args, "tor-image", "Development public Tor image digest", "", "Repository Tor image contract; public identity is kept separate from the administrator onion.", image); err != nil {
				return nil, "", err
			}
			if err = w.onionRole(&args, ""); err != nil {
				return nil, "", err
			}
		}
		admin := mode == "prod"
		if !admin {
			admin, err = w.yes("admin-tor", "Add client-authorized private administrator Tor? y/n", "A separate private Unix socket and authorized client keys protect administrator ingress.")
			if err != nil {
				return nil, "", err
			}
		}
		if admin {
			if _, err = w.flag(&args, "admin-tor-image", "Private administrator Tor image digest", "", "Tor uses the configured nonroot UID/GID. No database/app/wallet secrets are mounted there.", image); err != nil {
				return nil, "", err
			}
			if _, err = w.flag(&args, "admin-client-auth-dir", "Public administrator client .auth directory", "", "1–256 canonical descriptor:x25519 public records only. Client private keys stay on the administrator workstation.", directory); err != nil {
				return nil, "", err
			}
			if err = w.onionRole(&args, "admin-"); err != nil {
				return nil, "", err
			}
		}
	}
	return args, dir, nil
}

func (w *wizard) onionRole(args *[]string, role string) error {
	label := "Public"
	if role != "" {
		label = "Administrator"
	}
	choice, err := w.ask(role+"identity", label+" identity: 1 new random/prefix, 2 import retained identity", "1", "A new identity changes its address. Import an existing service's full address and matching native Tor files; no live keys are replaced.", oneOf("1", "2"))
	if err != nil {
		return err
	}
	if choice == "1" {
		_, err = w.flag(args, role+"onion-prefix", label+" optional vanity prefix", "", "At most 6 lowercase base32 characters. Six characters average about 1.07 billion candidates; completion time is unpredictable. Blank generates a random identity.", prefix)
		return err
	}
	for _, f := range []struct {
		key, label string
		check      func(string) error
	}{
		{role + "onion-address", label + " complete retained .onion address", onionidentity.ValidateAddress},
		{role + "onion-public-key", label + " Tor hs_ed25519_public_key file", file(false)},
		{role + "onion-secret-key", label + " private Tor hs_ed25519_secret_key file", file(true)},
	} {
		if _, err = w.flag(args, f.key, f.label, "", "All three inputs must match. Keep service keys distinct from browser/client authorization keys.", f.check); err != nil {
			return err
		}
	}
	return nil
}

func (w *wizard) onion() error {
	choice, err := w.ask("onion-action", "Onion identity: 1 generate, 2 import", "1", "Creates local native Tor identity files only. Does not publish a service or change a running Tor process.", oneOf("1", "2"))
	if err != nil {
		return err
	}
	action := "generate"
	if choice == "2" {
		action = "import"
	}
	args := []string{action, "onion"}
	dir, err := w.flag(&args, "out-dir", "New private onion identity directory", "", "Parent must already exist. Existing targets are never replaced.", newDirectory)
	if err != nil {
		return err
	}
	if action == "generate" {
		if _, err = w.flag(&args, "prefix", "Optional vanity prefix", "", "Blank is random. At most 6 lowercase base32 characters; six averages 1,073,741,824 candidates, with no speed guarantee.", prefix); err != nil {
			return err
		}
		if _, err = w.flag(&args, "workers", "Search workers (0=automatic, at most 8)", "0", "Use 0 for at most 8 workers, or an explicit 1..64; Ctrl-C cancels searching.", func(v string) error {
			n, e := strconv.Atoi(v)
			if e != nil || n < 0 || n > 64 {
				return errors.New("Use 0..64 workers.")
			}
			return nil
		}); err != nil {
			return err
		}
	} else {
		for _, f := range []struct {
			key, label string
			check      func(string) error
		}{{"address", "Complete expected .onion address", onionidentity.ValidateAddress}, {"public-key", "Tor public key file", file(false)}, {"secret-key", "Private 0600 Tor secret key file", file(true)}} {
			if _, err = w.flag(&args, f.key, f.label, "", "Native Tor files must match the complete expected address.", f.check); err != nil {
				return err
			}
		}
	}
	preview(w.out, "opsecmkt keys", args)
	confirmed, err := w.yes("onion-confirm", "Create these private identity files? y/n", "Default n cancels before generation/import writes.")
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(w.out, "Cancelled; no identity created.")
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.searchNotice(args)
	if err := w.run.keys(w.ctx, args, w.out); err != nil {
		return err
	}
	fmt.Fprintf(w.out, "Back up %s encrypted/offline with hostname and both Tor key files. Keep public/admin service identities distinct. A running Tor service needs its private key online; generation alone publishes nothing.\n", dir)
	return nil
}

func (w *wizard) manage() error {
	dir, err := w.ask("existing-dir", "Existing private installation directory", "./opsecmkt-deployment", "Keep its original config, project/context identity, endpoints and stable keys.", directory)
	if err != nil {
		return err
	}
	action, err := w.ask("manage-action", "Action: plan, status, up, down, redeploy", "plan", "plan/status inspect state. up resumes configured services. down retains volumes. redeploy targets only app; review migrations and backups first.", oneOf("plan", "status", "up", "down", "redeploy"))
	if err != nil {
		return err
	}
	args := []string{action, "--dir", dir}
	if action == "redeploy" {
		if _, err = w.flag(&args, "app-image", "Reviewed replacement app image digest", "", "Check migrations, backups and compatibility. Existing database/payment/upload/version worker hosts and keys stay unchanged.", image); err != nil {
			return err
		}
	}
	preview(w.out, "opsecmkt deploy", args)
	if action != "plan" && action != "status" {
		yes, err := w.yes("manage-confirm", "Run the reviewed lifecycle action? y/n", "Default n cancels. down stops selected services; redeploy can run database migrations. No automatic rollback.")
		if err != nil {
			return err
		}
		if !yes {
			fmt.Fprintln(w.out, "Cancelled; no lifecycle action run.")
			return nil
		}
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	return w.run.deploy(w.ctx, args, w.in, w.out, w.errOut)
}
