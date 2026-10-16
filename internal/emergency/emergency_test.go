package emergency

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opsecmkt/internal/operator"
)

type fakeHost struct {
	events               *[]string
	failCheck, failErase bool
}

func (h fakeHost) checkDevice(context.Context, string) error {
	*h.events = append(*h.events, "check")
	if h.failCheck {
		return errors.New("not LUKS")
	}
	return nil
}
func (h fakeHost) erase(context.Context, string) error {
	*h.events = append(*h.events, "erase")
	if h.failErase {
		return errors.New("erase failed")
	}
	return nil
}
func (h fakeHost) reboot() error { *h.events = append(*h.events, "reboot"); return nil }

func incidentDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), []byte(`{"project":"test-incident","docker_context":"default"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), []byte(`{"version":1,"project":"test-incident","docker_context":"default"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEmergencyRequiresExactTargetsAndStopsBeforeErasure(t *testing.T) {
	dir := incidentDir(t)
	base := options{command: "erase-luks", dir: dir, confirmProject: "test-incident", device: "/dev/test-luks", confirmDevice: "/dev/test-luks", reboot: true}
	for _, test := range []struct {
		name   string
		change func(*options)
	}{
		{"project", func(o *options) { o.confirmProject = "another-project" }},
		{"device", func(o *options) { o.confirmDevice = "/dev/another-device" }},
		{"placeholder", func(o *options) { o.device = "/dev/<ENCRYPTED_VOLUME_DEVICE>"; o.confirmDevice = o.device }},
		{"path", func(o *options) { o.device = "/tmp/a-file"; o.confirmDevice = o.device }},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := base
			test.change(&o)
			var events []string
			stop := func(context.Context, string, operator.EmergencyTarget, io.Writer, io.Writer) error {
				events = append(events, "stop")
				return nil
			}
			if execute(context.Background(), o, io.Discard, io.Discard, fakeHost{events: &events}, stop, func(string) error { return nil }) == nil {
				t.Fatal("unsafe action accepted")
			}
			for _, event := range events {
				if event == "stop" || event == "erase" || event == "reboot" {
					t.Fatal("mutated on invalid target", events)
				}
			}
		})
	}
	var events []string
	stop := func(context.Context, string, operator.EmergencyTarget, io.Writer, io.Writer) error {
		events = append(events, "stop")
		return nil
	}
	if err := execute(context.Background(), base, io.Discard, io.Discard, fakeHost{events: &events}, stop, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "check,stop,check,erase,reboot" {
		t.Fatalf("unsafe ordering %s", got)
	}
}

func TestEmergencyStopFailureNeverErasesOrReboots(t *testing.T) {
	var events []string
	o := options{command: "erase-luks", dir: incidentDir(t), confirmProject: "test-incident", device: "/dev/test-luks", confirmDevice: "/dev/test-luks", reboot: true}
	stop := func(context.Context, string, operator.EmergencyTarget, io.Writer, io.Writer) error {
		events = append(events, "stop")
		return errors.New("Docker unreachable")
	}
	if execute(context.Background(), o, io.Discard, io.Discard, fakeHost{events: &events}, stop, func(string) error { return nil }) == nil {
		t.Fatal("ignored stop failure")
	}
	if got := strings.Join(events, ","); got != "check,stop" {
		t.Fatal("destructive work after failed stop", got)
	}
}

func TestEmergencyPlanDoesNotMutateAndEraseFailureDoesNotReboot(t *testing.T) {
	var events []string
	var out bytes.Buffer
	o := options{command: "plan", dir: incidentDir(t), device: "/dev/test-luks", reboot: true}
	stop := func(context.Context, string, operator.EmergencyTarget, io.Writer, io.Writer) error {
		events = append(events, "stop")
		return nil
	}
	if err := execute(context.Background(), o, &out, io.Discard, fakeHost{events: &events}, stop, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "check" || !strings.Contains(out.String(), "keyslots") {
		t.Fatal("bad plan", events, out.String())
	}
	events = nil
	o.command = "erase-luks"
	o.confirmProject = "test-incident"
	o.confirmDevice = o.device
	if execute(context.Background(), o, io.Discard, io.Discard, fakeHost{events: &events, failErase: true}, stop, func(string) error { return nil }) == nil {
		t.Fatal("ignored erasure failure")
	}
	if strings.Contains(strings.Join(events, ","), "reboot") {
		t.Fatal("rebooted after failed erasure")
	}
}
