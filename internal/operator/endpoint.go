package operator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const endpointUsage = `Usage: opsecmkt deploy endpoint set --dir DIR --database-url-file FILE [--database-ca FILE]
Replaces only the imported external PostgreSQL URL (and, with --database-ca, its CA)
of a stopped installation, for example after a restore, point-in-time recovery or
rollback. The URL needs sslmode=verify-full. Every other key, endpoint, pin and the
installation receipt are kept. Starts nothing; run deploy down first.
`

// runEndpoint is the explicit, operator-invoked replacement of the database
// endpoint (A-695). Redeployment never changes it as a side effect.
func runEndpoint(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, endpointUsage)
		return err
	}
	if args[0] != "set" {
		return errors.New("unknown endpoint command; run deploy endpoint help")
	}
	f := flag.NewFlagSet("deploy endpoint set", flag.ContinueOnError)
	f.SetOutput(errOut)
	directory := f.String("dir", "./opsecmkt-deployment", "existing private installation directory")
	urlFile := f.String("database-url-file", "", "0600 file holding the replacement PostgreSQL URL; sslmode=verify-full")
	caFile := f.String("database-ca", "", "optional replacement PostgreSQL TLS CA PEM; otherwise the imported CA is kept")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *urlFile == "" {
		return errors.New("endpoint set requires --database-url-file naming a private file; nothing was changed")
	}
	dir, c, unlock, err := openReplacement(ctx, *directory, []string{*urlFile})
	if err != nil {
		return err
	}
	defer unlock()
	if c.PostgresImage != "" {
		return errors.New("this development installation uses its own Compose PostgreSQL; endpoint set replaces only an imported external database URL; nothing was changed")
	}
	raw, err := privateFile(*urlFile, 64<<10)
	if err != nil {
		return fmt.Errorf("DATABASE_URL import: %w; nothing was changed", err)
	}
	value := strings.TrimSpace(string(raw))
	u, err := url.Parse(value)
	if err != nil || !(u.Scheme == "postgres" || u.Scheme == "postgresql") || u.Hostname() == "" || u.Query().Get("sslmode") != "verify-full" {
		return errors.New("replacement database URL must be a postgres:// URL with a host and sslmode=verify-full; nothing was changed")
	}
	stage := newReplacementStage(dir, c)
	defer stage.discard()
	caName := c.TLSFiles["DATABASE_CA"]
	if *caFile != "" {
		b, err := readImport(*caFile, false)
		if err != nil {
			return err
		}
		if caName == "" {
			caName = filepath.Join("secrets", "database-ca.pem")
		}
		if err := stage.add(stage.candidate.TLSFiles, "DATABASE_CA", caName, b); err != nil {
			return err
		}
	}
	if caName != "" {
		if value, err = databaseURLWithCA(value, "/run/opsecmkt-secrets/"+filepath.Base(caName)); err != nil {
			return err
		}
	}
	if err := stage.add(stage.candidate.SecretFiles, "DATABASE_URL", c.SecretFiles["DATABASE_URL"], []byte(value+"\n")); err != nil {
		return err
	}
	if err := validate(dir, stage.candidate); err != nil {
		return err
	}
	if err := validateCAMaterial(dir, stage.candidate); err != nil {
		return err
	}
	// A newly imported CA is the only configuration change; file names stay fixed.
	var updated *Config
	if c.TLSFiles["DATABASE_CA"] == "" && *caFile != "" {
		c.TLSFiles["DATABASE_CA"] = caName
		updated = &c
	}
	if err := stage.commit(updated); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "database endpoint replaced; every other key, endpoint, pin and the installation receipt were kept; nothing was started. Apply the restore gate if the database needs it, then run deploy redeploy or deploy up")
	return err
}

// openReplacement loads the installation under its lock and refuses unless the
// application is stopped and production storage, including the private
// imports, still matches the recorded encrypted bindings.
func openReplacement(ctx context.Context, directory string, imports []string) (string, Config, func(), error) {
	dir, err := installationPath(directory)
	if err != nil {
		return "", Config{}, nil, err
	}
	if _, err := loadConfig(dir); err != nil {
		return "", Config{}, nil, err
	}
	unlock, err := lockInstallation(dir)
	if err != nil {
		return "", Config{}, nil, err
	}
	fail := func(err error) (string, Config, func(), error) {
		unlock()
		return "", Config{}, nil, err
	}
	c, err := loadConfig(dir)
	if err != nil {
		return fail(err)
	}
	if c.Mode == "prod" {
		actual, err := productionStorageProbe(ctx, dir, c, imports)
		if err != nil {
			return fail(err)
		}
		if err := checkStoragePins(actual, c.StorageBindings); err != nil {
			return fail(err)
		}
	}
	if err := refuseApplicationContainer(ctx, c); err != nil {
		return fail(err)
	}
	if c.TLSFiles == nil {
		c.TLSFiles = map[string]string{}
	}
	return dir, c, unlock, nil
}

// refuseApplicationContainer requires the app container to be absent. Replaced
// files are read at container start, and Compose would not recreate an
// unchanged running container, so a stopped installation is the only state in
// which deploy up or redeploy is certain to use the replacement.
func refuseApplicationContainer(ctx context.Context, c Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "--context", c.DockerContext, "ps", "-aq", "--filter", "label=com.docker.compose.project="+c.Project, "--filter", "label=com.docker.compose.service=app")
	cmd.Env = dockerEnvironment()
	var output boundedOutput
	output.limit = 4096
	cmd.Stdout = &output
	cmd.Stderr = &output
	if cmd.Run() != nil || output.truncated {
		return errors.New("cannot verify that the application container is stopped; nothing was changed")
	}
	if strings.TrimSpace(string(output.data)) != "" {
		return errors.New("the application container exists; run deploy down first (it keeps every volume); nothing was changed")
	}
	return nil
}

// replacementStage writes candidate files beside the managed ones so the
// unchanged validators can check the would-be configuration before any
// managed file is replaced. Commit renames each file over its existing name.
type replacementStage struct {
	dir       string
	candidate Config
	renames   [][2]string
}

func newReplacementStage(dir string, c Config) *replacementStage {
	candidate := c
	candidate.SecretFiles = maps.Clone(c.SecretFiles)
	candidate.TLSFiles = maps.Clone(c.TLSFiles)
	if candidate.TLSFiles == nil {
		candidate.TLSFiles = map[string]string{}
	}
	return &replacementStage{dir: dir, candidate: candidate}
}

func (s *replacementStage) add(files map[string]string, key, final string, data []byte) error {
	if _, err := managedPath(s.dir, filepath.Dir(final)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Join(s.dir, filepath.Dir(final)), ".replace-")
	if err != nil {
		return errors.New("cannot stage replacement file")
	}
	name := temporary.Name()
	s.renames = append(s.renames, [2]string{name, filepath.Join(s.dir, final)})
	if err = temporary.Chmod(0600); err == nil {
		if _, err = temporary.Write(data); err == nil {
			err = temporary.Sync()
		}
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil && os.Geteuid() == 0 {
		err = os.Chown(name, s.candidate.RuntimeUID, s.candidate.RuntimeGID)
	}
	if err != nil {
		return errors.New("cannot stage replacement file")
	}
	files[key] = filepath.Join(filepath.Dir(final), filepath.Base(name))
	return nil
}

// commit replaces the managed files and, only when the configuration gained an
// entry (updated is non-nil), operator.json. A crash between renames leaves a
// mix that the next connection or start refuses (verify-full against the other
// CA, or a certificate/key mismatch); rerun the command.
func (s *replacementStage) commit(updated *Config) error {
	for _, rename := range s.renames {
		if info, err := os.Lstat(rename[1]); err == nil && !info.Mode().IsRegular() {
			return errors.New("refusing to replace a symlink or nonregular managed file")
		}
	}
	for _, rename := range s.renames {
		if err := os.Rename(rename[0], rename[1]); err != nil {
			return errors.New("could not replace a managed file; rerun the command after checking the installation")
		}
		if err := syncDirectory(filepath.Dir(rename[1])); err != nil {
			return err
		}
	}
	s.renames = nil
	if updated == nil {
		return nil
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "operator.json"), append(data, '\n'), 0600)
}

func (s *replacementStage) discard() {
	for _, rename := range s.renames {
		_ = os.Remove(rename[0])
	}
}
