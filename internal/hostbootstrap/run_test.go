package hostbootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanExportAndRefuseOverwrite(t *testing.T) {
	parent := t.TempDir()
	config := filepath.Join(parent, "host.json")
	b, _ := marshal(fixture())
	if e := os.WriteFile(config, b, 0600); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(parent, "plan")
	var out bytes.Buffer
	if e := Run(context.Background(), []string{"plan", "--config", config, "--out", dir}, &out, &out); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"host.json", "ingress.nft", "security-defaults.conf", "BOOTSTRAP.txt"} {
		info, e := os.Stat(filepath.Join(dir, name))
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private %s: %v", name, e)
		}
	}
	if e := Run(context.Background(), []string{"plan", "--config", config, "--out", dir}, &out, &out); e == nil {
		t.Fatal("overwrote plan")
	}
	if e := Run(context.Background(), []string{"apply", "--config", config}, &out, &out); e == nil || !strings.Contains(e.Error(), "operator-access-verified") {
		t.Fatal("missing access attestation accepted")
	}
}
func TestCommandRefusals(t *testing.T) {
	for _, args := range [][]string{{"mystery"}, {"rollback", "--config", "x"}, {"plan"}, {"apply", "--config", "missing"}, {"confirm", "--private-key", "secret"}} {
		var out bytes.Buffer
		if e := Run(context.Background(), args, &out, &out); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
