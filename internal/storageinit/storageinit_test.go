package storageinit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"opsecmkt/internal/storageguard"
)

const testUUID = "01234567-89ab-cdef-0123-456789abcdef"

type fakeVolume struct {
	calls []string
	fail  string
	uuid  string
}

func (v *fakeVolume) step(name string) error {
	v.calls = append(v.calls, name)
	if v.fail == name {
		return errors.New("injected refusal")
	}
	return nil
}
func (v *fakeVolume) Close() error                             { return v.step("close") }
func (v *fakeVolume) Format(context.Context, console) error    { return v.step("format") }
func (v *fakeVolume) Open(context.Context, console) error      { return v.step("open") }
func (v *fakeVolume) MakeFilesystem(context.Context) error     { return v.step("mkfs") }
func (v *fakeVolume) CheckFilesystem(context.Context) error    { return v.step("checkfs") }
func (v *fakeVolume) Mount(context.Context) error              { return v.step("mount") }
func (v *fakeVolume) WriteReceipt(string) error                { return v.step("receipt") }
func (v *fakeVolume) Cleanup(context.Context) error            { return v.step("cleanup") }
func (v *fakeVolume) Identity(context.Context) (string, error) { return v.uuid, v.step("identity") }

func prepareArgs() []string {
	return []string{"prepare", "--device", "/dev/disk/by-id/new-data", "--confirm-device", "/dev/vdb", "--name", "opsecmkt-state", "--mount", "/srv/opsecmkt-encrypted"}
}

func unlockArgs() []string {
	return []string{"unlock", "--device", "/dev/disk/by-id/data", "--expected-luks-uuid", testUUID, "--name", "opsecmkt-state", "--mount", "/srv/opsecmkt-encrypted"}
}

func fakePlatform(v *fakeVolume) platform {
	return platform{
		newVolume: func(context.Context, options, console) (volume, error) { return v, v.step("preflight") },
		inspect: func(path string) (storageguard.Evidence, error) {
			err := v.step("inspect")
			return storageguard.Evidence{Path: path, LUKSUUIDs: []string{testUUID}}, err
		},
	}
}

func TestPrepareExplicitOrderedStages(t *testing.T) {
	v := &fakeVolume{uuid: testUUID}
	var out bytes.Buffer
	if err := run(context.Background(), prepareArgs(), console{out: &out}, fakePlatform(v)); err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "format", "identity", "open", "mkfs", "mount", "inspect", "receipt", "close"}
	if !reflect.DeepEqual(v.calls, want) {
		t.Fatalf("operations = %v; want %v", v.calls, want)
	}
	if !strings.Contains(out.String(), testUUID) || !strings.Contains(out.String(), "No services were started") {
		t.Fatalf("missing non-secret completion evidence: %q", out.String())
	}
}

func TestUnlockNeverFormatsOrCreatesReceipt(t *testing.T) {
	v := &fakeVolume{uuid: testUUID}
	if err := run(context.Background(), unlockArgs(), console{}, fakePlatform(v)); err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "identity", "open", "checkfs", "mount", "inspect", "close"}
	if !reflect.DeepEqual(v.calls, want) {
		t.Fatalf("operations = %v; want %v", v.calls, want)
	}
}

func TestPreparationPreflightRefusalNeverFormats(t *testing.T) {
	v := &fakeVolume{uuid: testUUID, fail: "preflight"}
	if err := run(context.Background(), prepareArgs(), console{}, fakePlatform(v)); err == nil {
		t.Fatal("preflight refusal accepted")
	}
	if !reflect.DeepEqual(v.calls, []string{"preflight"}) {
		t.Fatalf("unsafe operations after refusal: %v", v.calls)
	}
}

func TestFailedStagesCleanUpOnlyTheirOwnMapping(t *testing.T) {
	for _, fail := range []string{"format", "identity", "open", "mkfs", "mount", "inspect", "receipt"} {
		t.Run(fail, func(t *testing.T) {
			v := &fakeVolume{uuid: testUUID, fail: fail}
			if err := run(context.Background(), prepareArgs(), console{}, fakePlatform(v)); err == nil {
				t.Fatal("failed stage accepted")
			}
			if got := v.calls[len(v.calls)-2:]; !reflect.DeepEqual(got, []string{"cleanup", "close"}) {
				t.Fatalf("missing cleanup: %v", v.calls)
			}
		})
	}
}

func TestUnlockRejectsIdentityBeforePromptOrMount(t *testing.T) {
	v := &fakeVolume{uuid: "11234567-89ab-cdef-0123-456789abcdef"}
	if err := run(context.Background(), unlockArgs(), console{}, fakePlatform(v)); err == nil {
		t.Fatal("wrong device UUID accepted")
	}
	want := []string{"preflight", "identity", "cleanup", "close"}
	if !reflect.DeepEqual(v.calls, want) {
		t.Fatalf("operations after wrong UUID: %v", v.calls)
	}
}

func TestMountedIdentityMismatchRejectsBeforeReceipt(t *testing.T) {
	v := &fakeVolume{uuid: testUUID}
	p := fakePlatform(v)
	p.inspect = func(string) (storageguard.Evidence, error) {
		v.step("inspect")
		return storageguard.Evidence{LUKSUUIDs: []string{"11234567-89ab-cdef-0123-456789abcdef"}}, nil
	}
	if err := run(context.Background(), prepareArgs(), console{}, p); err == nil {
		t.Fatal("different mounted identity accepted")
	}
	if strings.Contains(strings.Join(v.calls, ","), "receipt") {
		t.Fatal("wrote receipt after mismatched mounted UUID")
	}
}

func TestVerifyEveryPathWithExactPin(t *testing.T) {
	var paths []string
	p := platform{inspect: func(path string) (storageguard.Evidence, error) {
		paths = append(paths, path)
		return storageguard.Evidence{Path: path, LUKSUUIDs: []string{testUUID}}, nil
	}}
	if err := run(context.Background(), []string{"verify", "--path", "/", "--path", "/srv/state", "--expected-luks-uuid", testUUID}, console{}, p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/", "/srv/state"}) {
		t.Fatalf("paths = %v", paths)
	}
	p.inspect = func(string) (storageguard.Evidence, error) {
		return storageguard.Evidence{LUKSUUIDs: []string{testUUID, "11234567-89ab-cdef-0123-456789abcdef"}}, nil
	}
	if err := run(context.Background(), []string{"verify", "--path", "/srv/state", "--expected-luks-uuid", testUUID}, console{}, p); err == nil {
		t.Fatal("one UUID pin accepted a path backed by multiple identities")
	}
}

func TestVerifyRefusesUnprovenPath(t *testing.T) {
	p := platform{inspect: func(string) (storageguard.Evidence, error) {
		return storageguard.Evidence{}, errors.New("unproven storage")
	}}
	if err := run(context.Background(), []string{"verify", "--path", "/srv/state"}, console{}, p); err == nil {
		t.Fatal("failed proof accepted")
	}
	p.inspect = func(string) (storageguard.Evidence, error) { return storageguard.Evidence{}, nil }
	if err := run(context.Background(), []string{"verify", "--path", "/srv/state"}, console{}, p); err == nil {
		t.Fatal("empty identity proof accepted")
	}
}

func TestInvalidCLIInputsNeverReachDevicesOrEchoUnknownValues(t *testing.T) {
	for _, args := range [][]string{
		{"prepare"},
		{"unlock", "--device", "/dev/vdb", "--name", "state", "--mount", "/srv/state"},
		{"verify"},
		{"verify", "--path", "/srv/state", "--expected-luks-uuid", "ABC"},
		{"prepare", "--passphrase=secret-value-do-not-echo"},
		{"verify", "positional-secret-value-do-not-echo"},
		{"unsupported"},
	} {
		v := &fakeVolume{uuid: testUUID}
		var output bytes.Buffer
		err := run(context.Background(), args, console{out: &output, errOut: &output}, fakePlatform(v))
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
		if len(v.calls) != 0 {
			t.Fatalf("invalid flags reached device operations: %v", v.calls)
		}
		if strings.Contains(err.Error()+output.String(), "secret-value-do-not-echo") {
			t.Fatal("unknown flag or positional value was echoed")
		}
	}
}

func TestCancelledCommandNeverTouchesDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := &fakeVolume{uuid: testUUID}
	if err := run(ctx, prepareArgs(), console{}, fakePlatform(v)); err == nil || len(v.calls) != 0 {
		t.Fatalf("cancelled operation touched device: %v, %v", err, v.calls)
	}
}

func TestHelpNeedsNoPlatform(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"prepare", "--help"}, {"unlock", "--help"}, {"verify", "--help"}} {
		if err := run(context.Background(), args, console{out: io.Discard}, platform{}); err != nil {
			t.Fatal(err)
		}
	}
}
