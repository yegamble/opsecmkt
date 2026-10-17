package operator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"opsecmkt/internal/brandingconfig"
	"opsecmkt/internal/storageinit"
)

const usage = `Usage: opsecmkt deploy COMMAND [options]
Commands:
  host-reboot configure the private app-host reboot service; mark rehearsed unlock readiness explicitly
  storage   prepare/unlock an additional encrypted disk, verify storage or bind a migrated installation
  init      create private configuration and keys; starts nothing; never overwrites
  plan      validate and print a topology summary without secrets; starts nothing
  install   generate private Compose inputs and start the configured services
  up        resume/start the same installation without rotating keys
  redeploy  replace only the app image; preserve workers/endpoints/keys
  status    show this installation's container status
  down      stop/remove this installation's containers; retains every data volume
  logs      print the last 100 log lines (redacted; no follow)
  doctor    validate configuration and check Docker Compose availability
All commands accept --dir DIR (default ./opsecmkt-deployment).
Run deploy init --help for image, endpoint, key import and listener flags.
`

// Run executes a deployment command. The caller owns process exit codes.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	command := args[0]
	if command == "host-reboot" {
		return runHostReboot(ctx, args[1:], out, errOut)
	}
	if command == "storage" {
		if len(args) > 1 && (args[1] == "bind" || args[1] == "check-bound" || args[1] == "boot-unit") {
			return runBoundStorage(ctx, args[1:], out, errOut)
		}
		return storageinit.Run(ctx, args[1:], in, out, errOut)
	}
	if command == "init" {
		return initialize(ctx, args[1:], out, errOut)
	}
	switch command {
	case "plan", "install", "up", "redeploy", "status", "down", "logs", "doctor":
	default:
		return errors.New("unknown deployment command; run deploy --help")
	}
	flags := flag.NewFlagSet("deploy "+command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	directory := flags.String("dir", "./opsecmkt-deployment", "private installation directory")
	var appImage *string
	if command == "redeploy" {
		appImage = flags.String("app-image", "", "replacement app image including @sha256:digest")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if appImage != nil && !imagePattern.MatchString(*appImage) {
		return errors.New("redeploy requires --app-image with an explicit sha256 digest")
	}
	dir, err := installationPath(*directory)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if command == "plan" || command == "install" || command == "up" || command == "doctor" || command == "redeploy" {
		if err := validateMaterial(dir, cfg); err != nil {
			return err
		}
	}
	if command == "install" || command == "up" || command == "down" || command == "redeploy" {
		unlock, err := lockInstallation(dir)
		if err != nil {
			return err
		}
		defer unlock()
		cfg, err = loadConfig(dir)
		if err != nil {
			return err
		}
		if command != "down" {
			if err := refuseEmergencyRestart(dir); err != nil {
				return err
			}
		}
		if command != "down" {
			if err := checkProductionStorage(ctx, dir, cfg); err != nil {
				return err
			}
			if err := checkRetentionStorage(dir, cfg); err != nil {
				return err
			}
		}
	}
	if command == "redeploy" {
		if cfg.Mode == "prod" {
			if err := installProductionBootGuard(ctx, dir, cfg); err != nil {
				return err
			}
			if cfg.HostControlDir != "" {
				if err := installHostRebootService(ctx, dir, cfg); err != nil {
					return err
				}
			}
		}
		if _, err := privateFile(filepath.Join(dir, "installation.json"), 4096); err != nil {
			return errors.New("redeploy requires an existing installation; run install first")
		}
		if _, err := privateFile(filepath.Join(dir, "compose.json"), 2<<20); err != nil {
			return errors.New("redeploy requires existing runtime configuration; run install first")
		}
		cfg.AppImage = *appImage
		if err := validate(dir, cfg); err != nil {
			return err
		}
		compose, _, err := render(dir, cfg)
		if err != nil {
			return err
		}
		configJSON, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "operator.json"), append(configJSON, '\n'), 0600); err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "compose.json"), compose, 0600); err != nil {
			return err
		}
		if err := dockerCall(ctx, dir, cfg, out, errOut, "up", "-d", "--wait", "--wait-timeout", "600", "--no-deps", "app"); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "application redeployed; worker services, endpoints, keys and volumes retained")
		return err
	}
	if command == "plan" {
		return printPlan(out, cfg)
	}
	if command == "doctor" {
		if err := checkProductionStorage(ctx, dir, cfg); err != nil {
			return err
		}
		if err := dockerCall(ctx, dir, cfg, out, errOut, "version"); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "configuration valid; Docker Compose reachable; host separation and remote service readiness require deployment verification")
		return err
	}
	if command == "install" || command == "up" {
		if cfg.Mode == "prod" {
			if err := installProductionBootGuard(ctx, dir, cfg); err != nil {
				return err
			}
			if cfg.HostControlDir != "" {
				if err := installHostRebootService(ctx, dir, cfg); err != nil {
					return err
				}
			}
		}
		if err := claimInstallation(ctx, dir, cfg); err != nil {
			return err
		}
		if err := materialize(dir, cfg); err != nil {
			return err
		}
		if err := dockerCall(ctx, dir, cfg, out, errOut, "up", "-d", "--wait", "--wait-timeout", "600"); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "configured containers started; run deploy status and complete first-run administrator setup")
		return err
	}
	if command == "logs" && cfg.Mode == "prod" {
		_, err := fmt.Fprintln(out, "persistent container logs are disabled for the production application; use administrator diagnostics and deploy status")
		return err
	}
	if _, err := privateFile(filepath.Join(dir, "compose.json"), 2<<20); err != nil {
		return errors.New("runtime configuration is missing; run deploy install first")
	}
	switch command {
	case "status":
		return dockerCall(ctx, dir, cfg, out, errOut, "ps", "--all")
	case "down":
		return dockerCall(ctx, dir, cfg, out, errOut, "down", "--timeout", "60")
	case "logs":
		return dockerCall(ctx, dir, cfg, out, errOut, "logs", "--no-color", "--tail", "100")
	}
	return nil
}

type settingsFlags []string

func (s *settingsFlags) String() string     { return strings.Join(*s, ",") }
func (s *settingsFlags) Set(v string) error { *s = append(*s, v); return nil }

func initialize(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flag.NewFlagSet("deploy init", flag.ContinueOnError)
	f.SetOutput(errOut)
	dirFlag := f.String("dir", "./opsecmkt-deployment", "new private installation directory")
	dockerContext := f.String("docker-context", "default", "explicit Docker context; caller DOCKER_HOST/DOCKER_CONTEXT are ignored")
	mode := f.String("mode", "dev", "dev (single host) or prod (separate dependency hosts)")
	project := f.String("project", "", "stable Docker Compose project name; default generates a unique name")
	appImage := f.String("app-image", "", "required reviewed application image reference including @sha256:digest")
	pgImage := f.String("postgres-image", postgresImage, "development PostgreSQL image including digest; unused with external database")
	torImage := f.String("tor-image", "", "development Tor image including digest (same contract as deploy/Dockerfile.tor)")
	adminTorImage := f.String("admin-tor-image", "", "private administrator Tor image including digest; required for production Tor")
	adminAuthSource := f.String("admin-client-auth-dir", "", "directory of public descriptor:x25519 .auth records; validated and copied into a managed snapshot")
	hostControlDir := f.String("host-control-dir", "", "optional /run/opsecmkt-host-control-PROJECT socket directory for private admin app-host reboot control")
	onion := addOnionFlags(f, "")
	adminOnion := addOnionFlags(f, "admin-")
	retentionDir := f.String("retention-dir", "", "persistent absolute retention ledger directory; default INSTALL_DIR/retention, must be external when installation is on tmpfs")
	clamImage := f.String("clamav-image", "", "development ClamAV image including digest (must include signatures)")
	moneroImage := f.String("monero-image", "", "development monerod image including digest")
	walletImage := f.String("monero-wallet-image", "", "development monero-wallet-rpc image including digest")
	brandingOrigin := f.String("branding-upload-origin", "", "fixed public upload gateway origin for logo handoff; HTTPS or HTTP v3 onion; blank keeps the existing topology")
	siteAccess := f.String("site-access", "public", "initial visitor access: public or members; saved administrator settings take precedence after first start")
	registrations := f.String("registrations", "open", "initial new registrations: open or closed; saved administrator settings take precedence after first start")
	exposure := f.String("exposure", "clearnet", "clearnet or tor; production Tor ingress runs on the gateway host")
	bind := f.String("app-bind", "127.0.0.1:8080", "host app listener; explicit loopback/private IP:port")
	backendBind := f.String("backend-bind", "127.0.0.1:8443", "production upload backend listener; explicit private IP:port")
	appHost := f.String("app-host", "", "production application host identity for topology checks")
	gateway := f.String("gateway-url", "", "production gateway public HTTPS URL or HTTP onion URL")
	network := f.String("monero-network", "stagenet", "stagenet or mainnet; dev requires stagenet")
	escrow := f.Bool("escrow-xmr", false, "enable custodial Monero escrow; requires seed public key and daemon/wallet endpoints")
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 65532
	}
	if gid == 0 {
		gid = 65532
	}
	runtimeUID := f.Int("runtime-uid", uid, "application UID; defaults to operator UID, or 65532 when root")
	runtimeGID := f.Int("runtime-gid", gid, "application GID; defaults to operator GID, or 65532 when root")
	dbCA := f.String("database-ca", "", "optional PostgreSQL TLS CA PEM; imported and mounted for sslrootcert")
	dbFile := f.String("database-url-file", "", "0600 file holding external PostgreSQL URL; prod needs sslmode=verify-full")
	walletFile := f.String("monero-wallet-rpc-url-file", "", "0600 file holding the dedicated escrow wallet RPC URL")
	daemonFile := f.String("monero-daemon-url-file", "", "0600 file holding the restricted daemon RPC URL")
	appKey := f.String("app-secret-key-file", "", "import existing APP_SECRET_KEY from private file; otherwise generate")
	setupKey := f.String("setup-token-file", "", "import first-run setup token; otherwise generate")
	auditKey := f.String("audit-signing-key-file", "", "import Ed25519 signing seed (64 hex); otherwise generate")
	moneroKey := f.String("monero-wallet-secret-file", "", "import stable Monero wallet secret; otherwise generate when escrow is on")
	seedKey := f.String("monero-seed-key", "", "armored OpenPGP public seed-encryption key; never import private half")
	auditRecipient := f.String("audit-recipient-key", "", "optional armored OpenPGP public audit-export encryption key")
	uploadCA := f.String("upload-ca", "", "production upload backend client CA PEM (public certificate only)")
	uploadCert := f.String("upload-cert", "", "production upload backend server certificate PEM")
	uploadKey := f.String("upload-key", "", "production upload backend private key PEM; 0600")
	appTLSCert := f.String("tls-cert", "", "application HTTPS server certificate PEM")
	appTLSKey := f.String("tls-key", "", "application HTTPS private key PEM; 0600")
	moneroCA := f.String("monero-rpc-ca", "", "optional Monero HTTPS private CA PEM")
	moneroCert := f.String("monero-rpc-cert", "", "optional Monero HTTPS client certificate PEM")
	moneroTLSKey := f.String("monero-rpc-key", "", "optional Monero HTTPS client key PEM; 0600")
	dependencyDir := f.String("dependency-report-dir", "", "read-only public report directory; keep worker signing keys elsewhere")
	dependencyKey := f.String("dependency-report-public-key", "", "independently pinned 64-hex Ed25519 worker public key file")
	var settings settingsFlags
	f.Var(&settings, "set", "additional non-secret runtime setting KEY=VALUE; repeatable")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *mode != "dev" && *mode != "prod" {
		return errors.New("mode must be dev or prod; no configuration or keys were created")
	}
	dir, err := installationPath(*dirFlag)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return errors.New("installation directory already exists or is inaccessible; refusing to overwrite configuration or keys")
	}
	if !imagePattern.MatchString(*appImage) {
		return errors.New("--app-image must include an explicit sha256 digest")
	}
	if *project == "" {
		suffix, err := randomSecret()
		if err != nil {
			return err
		}
		*project = "opsecmkt-" + suffix[:12]
	}
	cfg := Config{DockerContext: *dockerContext, Version: configVersion, Mode: *mode, Project: *project, AppImage: *appImage, Exposure: *exposure, AppBind: *bind, AppHost: *appHost, GatewayURL: *gateway, RuntimeUID: *runtimeUID, RuntimeGID: *runtimeGID, RetentionDir: filepath.Join(dir, "retention"),
		Settings: map[string]string{"MONERO_NETWORK": *network, "ESCROW_XMR": "off", "RETENTION_ENABLED": "off", "INITIAL_SITE_ACCESS": *siteAccess, "INITIAL_REGISTRATIONS": *registrations, "BRANDING_UPLOAD_ORIGIN": *brandingOrigin}, SecretFiles: map[string]string{}, PublicFiles: map[string]string{}, TLSFiles: map[string]string{}}
	cfg.HostControlDir = *hostControlDir
	if *retentionDir != "" {
		cfg.RetentionDir = *retentionDir
	}
	if (*adminTorImage == "") != (*adminAuthSource == "") {
		return errors.New("private administrator onion requires both --admin-tor-image and --admin-client-auth-dir")
	}
	if *adminTorImage != "" {
		cfg.AdminTorImage, cfg.AdminAuthDir, cfg.AdminSocketDir = *adminTorImage, "admin-clients", filepath.Join(dir, "admin")
	}
	if *dependencyDir != "" {
		absolute, err := filepath.Abs(*dependencyDir)
		if err != nil {
			return err
		}
		cfg.DependencyReportDir = absolute
	}
	if *escrow {
		cfg.Settings["ESCROW_XMR"] = "on"
	}
	for _, setting := range settings {
		key, val, ok := strings.Cut(setting, "=")
		if !ok || !settingKeys[key] {
			return errors.New("--set requires a supported non-secret KEY=VALUE")
		}
		cfg.Settings[key] = val
	}
	if cfg.Settings["BRANDING_UPLOAD_ORIGIN"], err = brandingconfig.ParseOrigin(cfg.Settings["BRANDING_UPLOAD_ORIGIN"], cfg.Mode == "dev"); err != nil {
		return err
	}
	if *mode == "dev" {
		cfg.TorImage = *torImage
		cfg.ClamavImage = *clamImage
		cfg.MoneroImage = *moneroImage
		cfg.MoneroWalletImage = *walletImage
		if *dbFile == "" {
			cfg.PostgresImage = *pgImage
		}
	} else if *torImage != "" || *clamImage != "" || *moneroImage != "" || *walletImage != "" {
		return errors.New("production dependency images are refused; run those layers on their own hosts")
	}
	if *mode == "prod" {
		cfg.BackendBind = *backendBind
	}
	if os.Geteuid() != 0 && (*runtimeUID != os.Getuid() || *runtimeGID != os.Getgid()) {
		return errors.New("a nonroot operator must use its own runtime UID and GID so private mounted files remain readable")
	}
	if cfg.Mode == "prod" {
		// Verify the host before staging or importing the first private byte.
		// A sibling staging directory otherwise writes keys onto a plain root.
		imports := []string{*dbFile, *walletFile, *daemonFile, *appKey, *setupKey, *auditKey, *moneroKey,
			*uploadKey, *appTLSKey, *moneroTLSKey, *onion.secretKey, *adminOnion.secretKey}
		cfg.StorageBindings, err = productionStorageProbe(ctx, dir, cfg, imports)
		if err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".opsecmkt-init-")
	if err != nil {
		return errors.New("cannot stage installation beside the target directory")
	}
	defer os.RemoveAll(stage)
	if err := os.Mkdir(filepath.Join(stage, "secrets"), 0700); err != nil {
		return err
	}
	if cfg.AdminTorImage != "" {
		if err := importAdminAuth(*adminAuthSource, filepath.Join(stage, cfg.AdminAuthDir)); err != nil {
			return err
		}
	}
	writeSecret := func(key, value string) error {
		name := filepath.Join("secrets", strings.ToLower(strings.ReplaceAll(key, "_", "-")))
		if err := writeExclusive(filepath.Join(stage, name), []byte(value+"\n"), 0600); err != nil {
			return err
		}
		cfg.SecretFiles[key] = name
		return nil
	}
	importOrGenerate := func(key, source string) error {
		var value string
		if source != "" {
			b, err := privateFile(source, 64<<10)
			if err != nil {
				return fmt.Errorf("%s import: %w", key, err)
			}
			value = strings.TrimSpace(string(b))
		} else {
			var err error
			value, err = randomSecret()
			if err != nil {
				return errors.New("secure random key generation failed")
			}
		}
		return writeSecret(key, value)
	}
	for _, entry := range []struct{ key, source string }{{"APP_SECRET_KEY", *appKey}, {"SETUP_TOKEN", *setupKey}, {"AUDIT_SIGNING_KEY", *auditKey}} {
		if err := importOrGenerate(entry.key, entry.source); err != nil {
			return err
		}
	}
	if *dbFile != "" {
		if err := importOrGenerate("DATABASE_URL", *dbFile); err != nil {
			return err
		}
	} else {
		password, err := randomSecret()
		if err != nil {
			return err
		}
		if err := writeSecret("POSTGRES_PASSWORD", password); err != nil {
			return err
		}
		if err := writeSecret("DATABASE_URL", "postgres://opsecmkt:"+password+"@db:5432/opsecmkt?sslmode=disable"); err != nil {
			return err
		}
	}
	if cfg.Settings["ESCROW_XMR"] == "on" {
		if err := importOrGenerate("MONERO_WALLET_SECRET", *moneroKey); err != nil {
			return err
		}
		if *moneroImage != "" {
			if *walletFile != "" || *daemonFile != "" {
				return errors.New("choose either local Monero images or imported Monero endpoints")
			}
			password, err := randomSecret()
			if err != nil {
				return err
			}
			if err := writeSecret("ESCROW_XMR_WALLET_RPC_PASSWORD", password); err != nil {
				return err
			}
			if err := writeSecret("ESCROW_XMR_WALLET_RPC_URL", "http://escrow:"+password+"@monero-wallet:18083/json_rpc"); err != nil {
				return err
			}
			if err := writeSecret("ESCROW_XMR_DAEMON_URL", "http://monero:18081/json_rpc"); err != nil {
				return err
			}
		} else {
			if *walletFile == "" || *daemonFile == "" {
				return errors.New("Monero escrow needs daemon and wallet RPC URL files")
			}
			if err := importOrGenerate("ESCROW_XMR_WALLET_RPC_URL", *walletFile); err != nil {
				return err
			}
			if err := importOrGenerate("ESCROW_XMR_DAEMON_URL", *daemonFile); err != nil {
				return err
			}
		}
	}
	for _, entry := range []struct{ key, source string }{{"ESCROW_XMR_SEED_KEY", *seedKey}, {"AUDIT_EXPORT_RECIPIENT", *auditRecipient}, {"DEPENDENCY_REPORT_PUBLIC_KEY", *dependencyKey}} {
		if entry.source == "" {
			continue
		}
		var b []byte
		var err error
		if entry.key == "DEPENDENCY_REPORT_PUBLIC_KEY" {
			b, err = readImport(entry.source, false)
			if err == nil {
				err = validateWorkerPublicKey(b)
			}
		} else {
			b, err = readPublicKey(entry.source)
		}
		if err != nil {
			return err
		}
		name := filepath.Join("secrets", strings.ToLower(strings.ReplaceAll(entry.key, "_", "-"))+".asc")
		if err := writeExclusive(filepath.Join(stage, name), b, 0600); err != nil {
			return err
		}
		cfg.PublicFiles[entry.key] = name
	}
	for _, entry := range []struct{ key, source string }{{"DATABASE_CA", *dbCA}, {"UPLOAD_CA", *uploadCA}, {"UPLOAD_CERT", *uploadCert}, {"UPLOAD_KEY", *uploadKey}, {"TLS_CERT", *appTLSCert}, {"TLS_KEY", *appTLSKey}, {"MONERO_RPC_CA", *moneroCA}, {"MONERO_RPC_CERT", *moneroCert}, {"MONERO_RPC_KEY", *moneroTLSKey}} {
		if entry.source == "" {
			continue
		}
		b, err := readImport(entry.source, strings.HasSuffix(entry.key, "KEY"))
		if err != nil {
			return err
		}
		name := filepath.Join("secrets", strings.ToLower(strings.ReplaceAll(entry.key, "_", "-"))+".pem")
		if err := writeExclusive(filepath.Join(stage, name), b, 0600); err != nil {
			return err
		}
		cfg.TLSFiles[entry.key] = name
	}
	if *dbCA != "" {
		if cfg.PostgresImage != "" {
			return errors.New("database-ca is only supported with an external database")
		}
		value, err := secretValue(stage, cfg, "DATABASE_URL")
		if err != nil {
			return err
		}
		u, err := url.Parse(value)
		if err != nil {
			return errors.New("database URL is invalid")
		}
		query := u.Query()
		query.Set("sslrootcert", "/run/opsecmkt-secrets/database-ca.pem")
		u.RawQuery = query.Encode()
		if err := os.WriteFile(filepath.Join(stage, cfg.SecretFiles["DATABASE_URL"]), []byte(u.String()+"\n"), 0600); err != nil {
			return err
		}
	}
	if err := validate(stage, cfg); err != nil {
		return err
	}
	if err := validateMaterial(stage, cfg); err != nil {
		return err
	}
	// Validate the deployment before starting an optional long vanity search.
	cfg.OnionDir, cfg.OnionAddress, err = initializeOnionIdentity(ctx, stage, "", cfg.TorImage != "", onion)
	if err != nil {
		return err
	}
	cfg.AdminOnionDir, cfg.AdminOnionAddress, err = initializeOnionIdentity(ctx, stage, "admin-", cfg.AdminTorImage != "", adminOnion)
	if err != nil {
		return err
	}
	if err := validateOnionIdentities(stage, cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeExclusive(filepath.Join(stage, "operator.json"), data, 0600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := filepath.Walk(filepath.Join(stage, "secrets"), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Chown(path, cfg.RuntimeUID, cfg.RuntimeGID)
		}); err != nil {
			return errors.New("cannot assign private runtime file ownership")
		}
	}
	// Claim the final path exclusively, then replace only our own empty directory.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return errors.New("installation directory appeared during initialization; nothing was overwritten")
	}
	// Publish the validated runtime directory before the configuration commit marker.
	publication := []string{"secrets"}
	if cfg.AdminAuthDir != "" {
		publication = append(publication, cfg.AdminAuthDir)
	}
	for _, identityDir := range []string{cfg.OnionDir, cfg.AdminOnionDir} {
		if identityDir != "" {
			publication = append(publication, identityDir)
		}
	}
	publication = append(publication, "operator.json")
	for _, name := range publication {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dir, name)); err != nil {
			return errors.New("could not publish staged installation; preserve any files already created and inspect the private directory")
		}
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(out, "private configuration and keys created; no services started; review deploy plan, then deploy install"); err != nil {
		return err
	}
	return printOnionPlan(out, cfg)
}

func installationPath(path string) (string, error) {
	dir, err := filepath.Abs(path)
	if err != nil || strings.ContainsAny(dir, "\x00\r\n$") || dir == string(filepath.Separator) {
		return "", errors.New("invalid installation directory")
	}
	return filepath.Clean(dir), nil
}
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("refusing to replace an existing runtime file")
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func printPlan(out io.Writer, c Config) error {
	mode := "development (single host; stagenet only)"
	if c.Mode == "prod" {
		mode = "production app host (remote database, Monero and upload gateway; verify host separation operationally)"
	}
	services := []string{"app"}
	if c.PostgresImage != "" {
		services = append(services, "db")
	}
	if c.TorImage != "" {
		services = append(services, "tor-identity-prepare", "tor")
	}
	if c.AdminTorImage != "" {
		services = append(services, "admin-tor-prepare", "tor-admin")
	}
	if c.ClamavImage != "" {
		services = append(services, "clamd")
	}
	if c.MoneroImage != "" {
		services = append(services, "monero", "monero-wallet")
	}
	siteAccess, registrations := c.Settings["INITIAL_SITE_ACCESS"], c.Settings["INITIAL_REGISTRATIONS"]
	if siteAccess == "" {
		siteAccess = "public"
	}
	if registrations == "" {
		registrations = "open"
	}
	_, err := fmt.Fprintf(out, "mode: %s\nproject: %s\nDocker context: %s\nservices: %s\napp listener: %s\nMonero network: %s; escrow: %s\ninitial visitor access: %s; new registrations: %s (saved administrator settings take precedence)\nsecret values: private files, omitted\n", mode, c.Project, c.DockerContext, strings.Join(services, ", "), c.AppBind, c.Settings["MONERO_NETWORK"], c.Settings["ESCROW_XMR"], siteAccess, registrations)
	if err != nil {
		return err
	}
	origin, err := brandingconfig.ParseOrigin(c.Settings["BRANDING_UPLOAD_ORIGIN"], c.Mode == "dev")
	if err != nil {
		return err
	}
	if origin != "" {
		if _, err := fmt.Fprintln(out, "branding upload origin:", origin); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Settings["ESCROW_XMR_TOR_RELAY"]) == "remote-tor" {
		if _, err := fmt.Fprintln(out, "Monero relay: operator declares remote Tor egress; proxy and firewall behavior require verification on the payment host"); err != nil {
			return err
		}
	}
	return printOnionPlan(out, c)
}

func dockerCall(ctx context.Context, dir string, cfg Config, out, errOut io.Writer, args ...string) error {
	duration := 2 * time.Minute
	if len(args) > 0 && args[0] == "up" {
		duration = 12 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	commandArgs := []string{"--context", cfg.DockerContext, "compose", "--project-name", cfg.Project, "--project-directory", dir, "--env-file", os.DevNull, "--file", filepath.Join(dir, "compose.json")}
	if len(args) == 1 && args[0] == "version" {
		commandArgs = []string{"--context", cfg.DockerContext, "compose", "version"}
	} else {
		commandArgs = append(commandArgs, args...)
	}
	cmd := exec.CommandContext(ctx, "docker", commandArgs...)
	cmd.Env = dockerEnvironment()
	cmd.Dir = dir
	var capture boundedOutput
	capture.limit = 2 << 20
	cmd.Stdout = &capture
	cmd.Stderr = &capture
	err := cmd.Run()
	result := string(capture.data)
	if capture.truncated {
		if newline := strings.LastIndexByte(result, '\n'); newline >= 0 {
			result = result[:newline+1]
		} else {
			result = ""
		}
	}
	keys := make([]string, 0, len(cfg.SecretFiles))
	for key := range cfg.SecretFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, valueErr := secretValue(dir, cfg, key)
		if valueErr != nil {
			continue
		}
		variants := []string{value}
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			username := parsed.User.Username()
			password, _ := parsed.User.Password()
			variants = append(variants, parsed.User.String())
			for _, part := range []string{username, password} {
				if part != "" {
					variants = append(variants, part, url.QueryEscape(part), url.PathEscape(part))
				}
			}
		}
		sort.Slice(variants, func(i, j int) bool { return len(variants[i]) > len(variants[j]) })
		for _, variant := range variants {
			if variant != "" {
				result = strings.ReplaceAll(result, variant, "[REDACTED "+key+"]")
			}
		}
	}
	if _, writeErr := io.WriteString(out, result); writeErr != nil {
		return writeErr
	}
	if capture.truncated {
		_, _ = fmt.Fprintln(errOut, "Docker output truncated after 2 MiB")
	}
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("Docker command exceeded its deadline or was cancelled; configuration and volumes were retained")
		}
		return errors.New("Docker command failed; configuration and volumes were retained")
	}
	return nil
}
func dockerEnvironment() []string {
	keys := []string{"PATH", "HOME", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "XDG_RUNTIME_DIR", "SYSTEMROOT", "TEMP", "TMP"}
	env := []string{"COMPOSE_ANSI=never", "COMPOSE_INTERACTIVE_NO_CLI=1"}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type boundedOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (w *boundedOutput) Write(b []byte) (int, error) {
	n := len(b)
	remaining := w.limit - len(w.data)
	if len(b) > remaining {
		b = b[:remaining]
		w.truncated = true
	}
	w.data = append(w.data, b...)
	return n, nil
}
func numericID(uid, gid int) string { return strconv.Itoa(uid) + ":" + strconv.Itoa(gid) }
