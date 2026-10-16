package operator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type receipt struct {
	DockerContext string `json:"docker_context"`
	Project       string `json:"project"`
	Version       int    `json:"version"`
}

func checkReceipt(dir string, c Config) error {
	file := filepath.Join(dir, "installation.json")
	if _, err := os.Lstat(file); os.IsNotExist(err) {
		return nil
	}
	data, err := privateFile(file, 4096)
	if err != nil {
		return err
	}
	var r receipt
	if json.Unmarshal(data, &r) != nil || r.Version != 1 || r.Project != c.Project || r.DockerContext != c.DockerContext {
		return errors.New("installation project identity changed; preserve the recorded project and data volumes")
	}
	return nil
}
func claimInstallation(ctx context.Context, dir string, c Config) error {
	path := filepath.Join(dir, "installation.json")
	if _, err := os.Lstat(path); err == nil {
		return checkReceipt(dir, c)
	} else if !os.IsNotExist(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, args := range [][]string{{"ps", "-aq", "--filter", "label=com.docker.compose.project=" + c.Project}, {"volume", "ls", "-q", "--filter", "label=com.docker.compose.project=" + c.Project}} {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--context", c.DockerContext}, args...)...)
		cmd.Env = dockerEnvironment()
		var output boundedOutput
		output.limit = 4096
		cmd.Stdout = &output
		cmd.Stderr = &output
		if cmd.Run() != nil {
			return errors.New("cannot verify that the Docker project is unused; no services were started")
		}
		if strings.TrimSpace(string(output.data)) != "" || output.truncated {
			return errors.New("Docker containers or volumes already use this project; choose another project; existing resources were retained")
		}
	}
	data, _ := json.Marshal(receipt{Project: c.Project, Version: 1, DockerContext: c.DockerContext})
	if err := writeExclusive(path, append(data, '\n'), 0600); err != nil {
		return errors.New("installation was claimed concurrently; retry with the existing configuration")
	}
	return nil
}
