package operator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type EmergencyTarget struct{ Project, DockerContext string }

func InstallationTarget(dir string) (EmergencyTarget, error) {
	path, err := installationPath(dir)
	if err != nil {
		return EmergencyTarget{}, err
	}
	c, err := loadEmergencyIdentity(path)
	if err != nil {
		return EmergencyTarget{}, err
	}
	return EmergencyTarget{Project: c.Project, DockerContext: c.DockerContext}, nil
}

// InstallationProject returns only the validated installation's public project
// identity, for an explicit operator confirmation before EmergencyStop.
func InstallationProject(dir string) (string, error) {
	target, err := InstallationTarget(dir)
	return target.Project, err
}
func refuseEmergencyRestart(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, "emergency.json")); !os.IsNotExist(err) {
		return errors.New("installation has an emergency stop marker; review its state and explicitly remove emergency.json before any install, up or redeploy")
	}
	return nil
}

// EmergencyStop is an explicitly confirmed operator action. The persistent stop
// marker is written under the same lock as install/up/redeploy before any kill.
// It never removes volumes, configuration, keys or the marker. Callers must not
// destroy secrets or disks unless this method succeeds.
func EmergencyStop(ctx context.Context, dir string, expected EmergencyTarget, out, errOut io.Writer) error {
	path, err := installationPath(dir)
	if err != nil {
		return err
	}
	unlock, err := lockInstallation(path)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := loadEmergencyIdentity(path)
	if err != nil {
		return err
	}
	if expected.Project != c.Project || expected.DockerContext != c.DockerContext {
		return errors.New("emergency target changed after confirmation; no containers were stopped")
	}
	if _, err := privateFile(filepath.Join(path, "installation.json"), 4096); err != nil {
		return errors.New("emergency stop requires an existing claimed installation")
	}
	if _, err := privateFile(filepath.Join(path, "compose.json"), 2<<20); err != nil {
		return errors.New("emergency stop requires existing private runtime configuration")
	}
	marker := filepath.Join(path, "emergency.json")
	if _, err := os.Lstat(marker); os.IsNotExist(err) {
		b, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
		if err := writeExclusive(marker, append(b, '\n'), 0600); err != nil {
			return err
		}
		if err := syncDirectory(path); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if _, err := privateFile(marker, 4096); err != nil {
		return err
	}
	if err := dockerCall(ctx, path, c, out, errOut, "kill"); err != nil {
		return err
	}
	if err := dockerCall(ctx, path, c, out, errOut, "down", "--timeout", "0", "--remove-orphans"); err != nil {
		return err
	}
	return confirmEmergencyContainersRemoved(ctx, c)
}

func confirmEmergencyContainersRemoved(ctx context.Context, c Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "--context", c.DockerContext, "ps", "-aq", "--filter", "label=com.docker.compose.project="+c.Project)
	command.Env = dockerEnvironment()
	var output boundedOutput
	output.limit = 4096
	command.Stdout = &output
	command.Stderr = &output
	if command.Run() != nil || output.truncated || strings.TrimSpace(string(output.data)) != "" {
		return errors.New("cannot confirm that every container of the emergency project was removed; secrets and disks must be retained")
	}
	return nil
}

// Stop/confirmation only need the pinned Docker target. Lost or unusable live
// keys must not prevent stopping an already running deployment. Optional secret
// paths are retained only for best-effort output redaction.
func loadEmergencyIdentity(dir string) (Config, error) {
	var c Config
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return c, errors.New("emergency installation must be a real private directory")
	}
	b, err := privateFile(filepath.Join(dir, "operator.json"), 1<<20)
	if err != nil {
		return c, err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil || json.Unmarshal(raw["project"], &c.Project) != nil || json.Unmarshal(raw["docker_context"], &c.DockerContext) != nil || !projectPattern.MatchString(c.Project) || !projectPattern.MatchString(c.DockerContext) {
		return c, errors.New("emergency installation project or Docker context is invalid")
	}
	if _, err := privateFile(filepath.Join(dir, "installation.json"), 4096); err != nil {
		return c, errors.New("emergency stop requires an existing claimed installation")
	}
	if err := checkReceipt(dir, c); err != nil {
		return c, err
	}
	var paths map[string]string
	if json.Unmarshal(raw["secret_files"], &paths) == nil {
		c.SecretFiles = make(map[string]string)
		for key, path := range paths {
			if secretKeys[key] {
				c.SecretFiles[key] = path
			}
		}
	}
	return c, nil
}
