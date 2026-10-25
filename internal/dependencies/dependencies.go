// Package dependencies polls a fixed set of official release sources in a
// separate worker and verifies its local signed reports without network access.
package dependencies

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var serviceNames = []string{"postgresql", "monerod", "monero-wallet-rpc", "tor", "clamav", "go"}
var numericVersion = regexp.MustCompile(`^[0-9]{1,6}(?:\.[0-9]{1,6}){1,3}$`)
var llamaBuild = regexp.MustCompile(`^b[1-9][0-9]{0,8}$`)
var torRelease = regexp.MustCompile(`href="tor-([0-9]+(?:\.[0-9]+){3})\.tar\.gz"`)

const reportLimit = 64 << 10

// InferenceRuntimeService is optional inventory. Merely updating the worker or
// configuring moderation never adds a release request for it.
const InferenceRuntimeService = "llama.cpp"

// Entry contains version strings only; never RPC addresses, credentials or hosts.
// Current is operator-reported inventory, not live service telemetry.
type Entry struct {
	Service   string    `json:"service"`
	Current   string    `json:"current,omitempty"`
	Latest    string    `json:"latest,omitempty"`
	Source    string    `json:"source"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}
type Report struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Entries     []Entry   `json:"entries"`
}
type envelope struct {
	Report    json.RawMessage `json:"report"`
	Signature string          `json:"signature"`
}

func sourceFor(service string) string {
	switch service {
	case "postgresql":
		return "https://www.postgresql.org/versions.json"
	case "monerod", "monero-wallet-rpc":
		return "https://api.github.com/repos/monero-project/monero/releases/latest"
	case "tor":
		return "https://dist.torproject.org/"
	case "clamav":
		return "https://api.github.com/repos/Cisco-Talos/clamav/releases/latest"
	case "go":
		return "https://go.dev/dl/?mode=json"
	case InferenceRuntimeService:
		return "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest"
	}
	return ""
}

// Normalize accepts numeric stable versions and the prefixes used by these
// projects. It rejects arbitrary text, credentials and prerelease suffixes.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"clamav-", "go", "v"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if !numericVersion.MatchString(s) {
		return ""
	}
	return s
}

// NormalizeFor preserves the existing numeric policy for the six original
// services. llama.cpp uses stable bNNN build tags, accepted only for that service.
func NormalizeFor(service, version string) string {
	if service != InferenceRuntimeService {
		return Normalize(version)
	}
	version = strings.TrimSpace(version)
	if !llamaBuild.MatchString(version) {
		return ""
	}
	return version
}

func NewerFor(service, latest, current string) bool {
	if service != InferenceRuntimeService {
		return Newer(latest, current)
	}
	latest, current = NormalizeFor(service, latest), NormalizeFor(service, current)
	if latest == "" || current == "" {
		return false
	}
	newBuild, _ := strconv.Atoi(latest[1:])
	oldBuild, _ := strconv.Atoi(current[1:])
	return newBuild > oldBuild
}

// Newer compares numeric release components without treating unknowns as old.
func Newer(latest, current string) bool {
	latest, current = Normalize(latest), Normalize(current)
	if latest == "" || current == "" {
		return false
	}
	a, b := strings.Split(latest, "."), strings.Split(current, ".")
	for i := 0; i < max(len(a), len(b)); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			bv, _ = strconv.Atoi(b[i])
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

type currents map[string]string

func (c currents) String() string { return "" }
func (c currents) Set(value string) error {
	service, version, ok := strings.Cut(value, "=")
	normalized := NormalizeFor(service, version)
	if !ok || sourceFor(service) == "" || normalized == "" {
		return errors.New("current must be a supported SERVICE=VERSION (numeric release, or llama.cpp=bNNN)")
	}
	if _, exists := c[service]; exists {
		return errors.New("current service may be supplied only once")
	}
	c[service] = normalized
	return nil
}

// Run executes the isolated release worker. With no --once it polls immediately
// and then periodically until ctx ends. It never installs or upgrades software.
func Run(ctx context.Context, args []string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	f := flag.NewFlagSet("versions-worker", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	reportPath := f.String("report", "", "signed report destination (mount its directory read-only in the app)")
	signingPath := f.String("signing-key", "", "dedicated private 0600 file with a 64-hex-character Ed25519 seed")
	once := f.Bool("once", false, "refresh once and exit")
	interval := f.Duration("interval", 24*time.Hour, "refresh interval, 1 hour to 7 days")
	socks := f.String("socks5", "", "explicit Tor SOCKS5 host:port; DNS is resolved through SOCKS (otherwise direct egress)")
	current := currents{}
	f.Var(current, "current", "operator-reported installed SERVICE=VERSION; repeat for each service")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(out)
			fmt.Fprintln(out, "usage: opsecmkt versions-worker --report FILE --signing-key FILE [--current SERVICE=VERSION] [--once]")
			f.PrintDefaults()
			return nil
		}
		return errors.New("versions-worker: invalid flags")
	}
	if f.NArg() != 0 || *reportPath == "" || *signingPath == "" || *interval < time.Hour || *interval > 7*24*time.Hour {
		return errors.New("versions-worker: --report and --signing-key required; interval must be 1 hour to 7 days")
	}
	reportAbs, reportErr := filepath.Abs(*reportPath)
	keyAbs, keyErr := filepath.Abs(*signingPath)
	if reportErr != nil || keyErr != nil || reportAbs == keyAbs {
		return errors.New("versions-worker: report and signing key paths must be distinct")
	}
	keyInfo, keyInfoErr := os.Stat(*signingPath)
	reportInfo, reportInfoErr := os.Stat(*reportPath)
	if keyInfoErr == nil && reportInfoErr == nil && os.SameFile(keyInfo, reportInfo) {
		return errors.New("versions-worker: report must not replace its signing key")
	}
	keyBytes, err := readBounded(*signingPath, true)
	if err != nil {
		return errors.New("versions-worker: cannot read private signing key (regular file, mode 0600, maximum 64 KiB)")
	}
	seed, err := hex.DecodeString(strings.TrimSuffix(string(keyBytes), "\n"))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("versions-worker: signing key must be a 64-hex-character Ed25519 seed")
	}
	key := ed25519.NewKeyFromSeed(seed)
	client, err := newClient(*socks)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	for {
		if ctx.Err() != nil {
			return nil
		}
		report := poll(ctx, client, current, time.Now().UTC())
		if ctx.Err() != nil {
			return nil
		}
		if err := writeReport(*reportPath, report, key); err != nil {
			return err
		}
		available := 0
		for _, entry := range report.Entries {
			if entry.Latest != "" {
				available++
			}
		}
		if _, err := fmt.Fprintf(out, "dependency report refreshed: %d/%d official release checks available; no software changed\n", available, len(report.Entries)); err != nil {
			return err
		}
		if *once {
			return nil
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func newClient(socks string) (*http.Client, error) {
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 5, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	if socks != "" {
		host, port, err := net.SplitHostPort(socks)
		n, parseErr := strconv.Atoi(port)
		if err != nil || host == "" || strings.ContainsAny(host, "/@?#") || parseErr != nil || n < 1 || n > 65535 {
			return nil, errors.New("versions-worker: socks5 must be host:port without credentials or a URL scheme")
		}
		tr.Proxy = http.ProxyURL(&url.URL{Scheme: "socks5h", Host: socks})
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func poll(ctx context.Context, client *http.Client, current map[string]string, now time.Time) Report {
	report := Report{Version: 1, GeneratedAt: now}
	cache := map[string]string{}
	services := append([]string{}, serviceNames...)
	if NormalizeFor(InferenceRuntimeService, current[InferenceRuntimeService]) != "" {
		services = append(services, InferenceRuntimeService)
	}
	for _, service := range services {
		source := sourceFor(service)
		latest, known := cache[source]
		if !known {
			req, _ := http.NewRequestWithContext(ctx, "GET", source, nil)
			req.Header.Set("User-Agent", "opsecmkt-versions-worker/1")
			req.Header.Set("Accept", "application/json, text/html;q=0.5")
			resp, err := client.Do(req)
			if err == nil {
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && readErr == nil && len(body) <= 2<<20 {
					latest, _ = parseLatest(service, body)
				}
			}
			cache[source] = latest
		}
		entry := Entry{Service: service, Current: current[service], Latest: latest, Source: source, CheckedAt: now}
		if latest == "" {
			entry.Error = "unavailable"
		}
		report.Entries = append(report.Entries, entry)
	}
	return report
}

func parseLatest(service string, body []byte) (string, error) {
	latest := ""
	selectVersion := func(candidate string) {
		candidate = NormalizeFor(service, candidate)
		if candidate != "" && (latest == "" || NewerFor(service, candidate, latest)) {
			latest = candidate
		}
	}
	switch service {
	case "postgresql":
		var versions []struct {
			Major, LatestMinor string
			Supported          bool
		}
		if json.Unmarshal(body, &versions) != nil {
			return "", errors.New("invalid PostgreSQL release feed")
		}
		for _, version := range versions {
			if version.Supported {
				selectVersion(version.Major + "." + version.LatestMinor)
			}
		}
	case "go":
		var versions []struct {
			Version string
			Stable  bool
		}
		if json.Unmarshal(body, &versions) != nil {
			return "", errors.New("invalid Go release feed")
		}
		for _, version := range versions {
			if version.Stable {
				selectVersion(version.Version)
			}
		}
	case "monerod", "monero-wallet-rpc", "clamav", InferenceRuntimeService:
		var version struct {
			Tag               string `json:"tag_name"`
			Draft, Prerelease bool
		}
		if json.Unmarshal(body, &version) != nil || version.Draft || version.Prerelease {
			return "", errors.New("invalid stable release feed")
		}
		selectVersion(version.Tag)
	case "tor":
		for _, match := range torRelease.FindAllSubmatch(body, -1) {
			selectVersion(string(match[1]))
		}
	default:
		return "", errors.New("unsupported dependency")
	}
	if latest == "" {
		return "", errors.New("stable release not found")
	}
	return latest, nil
}

// ReadVerified reads only local files. The app never contacts a release source.
// Old authenticated reports remain readable so the UI can show a stale warning.
func ReadVerified(path, publicPath string, now time.Time) (Report, error) {
	var report Report
	data, err := readBounded(path, false)
	if err != nil {
		return report, errors.New("dependency report unavailable")
	}
	publicBytes, err := readBounded(publicPath, false)
	if err != nil {
		return report, errors.New("dependency report verification key unavailable")
	}
	public, err := hex.DecodeString(strings.TrimSuffix(string(publicBytes), "\n"))
	if err != nil || len(public) != ed25519.PublicKeySize {
		return report, errors.New("dependency report verification key invalid")
	}
	var env envelope
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&env) != nil || d.Decode(new(any)) != io.EOF {
		return report, errors.New("dependency report invalid")
	}
	signature, err := hex.DecodeString(env.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(public), env.Report, signature) {
		return report, errors.New("dependency report signature invalid")
	}
	d = json.NewDecoder(bytes.NewReader(env.Report))
	d.DisallowUnknownFields()
	if d.Decode(&report) != nil || d.Decode(new(any)) != io.EOF || report.Version != 1 || report.GeneratedAt.IsZero() || report.GeneratedAt.After(now.Add(5*time.Minute)) || len(report.Entries) < len(serviceNames) || len(report.Entries) > len(serviceNames)+1 {
		return Report{}, errors.New("dependency report contents invalid")
	}
	seen := map[string]bool{}
	for _, entry := range report.Entries {
		if sourceFor(entry.Service) == "" || seen[entry.Service] || entry.Source != sourceFor(entry.Service) || (entry.Current != "" && NormalizeFor(entry.Service, entry.Current) != entry.Current) || (entry.Latest != "" && NormalizeFor(entry.Service, entry.Latest) != entry.Latest) || (entry.Service == InferenceRuntimeService && entry.Current == "") || (entry.Error != "" && entry.Error != "unavailable") || entry.CheckedAt.IsZero() || entry.CheckedAt.After(report.GeneratedAt) || entry.CheckedAt.Before(report.GeneratedAt.Add(-time.Minute)) || (entry.Latest == "") != (entry.Error == "unavailable") {
			return Report{}, errors.New("dependency report entry invalid")
		}
		seen[entry.Service] = true
	}
	for _, service := range serviceNames {
		if !seen[service] {
			return Report{}, errors.New("dependency report required service missing")
		}
	}
	return report, nil
}

func readBounded(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("file unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || (private && actual.Mode().Perm()&0077 != 0) {
		return nil, errors.New("file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, reportLimit+1))
	if err != nil || len(data) == 0 || len(data) > reportLimit {
		return nil, errors.New("file exceeds limit")
	}
	return data, nil
}

func writeReport(path string, report Report, key ed25519.PrivateKey) error {
	data, err := json.Marshal(report)
	if err != nil {
		return errors.New("versions-worker: cannot encode report")
	}
	data, err = json.Marshal(envelope{Report: data, Signature: hex.EncodeToString(ed25519.Sign(key, data))})
	if err != nil || len(data) > reportLimit {
		return errors.New("versions-worker: report exceeds limit")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("versions-worker: destination must not be a symlink or nonregular file")
		}
		existing, readErr := readBounded(path, false)
		var previous envelope
		decodeErr := json.Unmarshal(existing, &previous)
		sig, sigErr := hex.DecodeString(previous.Signature)
		if readErr != nil || decodeErr != nil || sigErr != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), previous.Report, sig) {
			return errors.New("versions-worker: existing destination is not a report signed by this key; use a new report path")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("versions-worker: destination unavailable")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dependency-report-*")
	if err != nil {
		return errors.New("versions-worker: cannot stage report")
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0644); err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("versions-worker: cannot write report")
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return errors.New("versions-worker: cannot publish report")
	}
	return nil
}
