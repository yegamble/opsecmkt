//go:build linux

package storageinit

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBlankDeviceTopologyFixtureAndRefusals(t *testing.T) {
	blank := deviceTopology{readOnly: "0", size: "2097152"}
	if err := validateWholeDevice(blank); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*deviceTopology){
		"partition":        func(d *deviceTopology) { d.partition = true },
		"root-disk-child":  func(d *deviceTopology) { d.partitions = 1 },
		"luks-holder":      func(d *deviceTopology) { d.holders = 1 },
		"stacked-device":   func(d *deviceTopology) { d.slaves = 1 },
		"read-only":        func(d *deviceTopology) { d.readOnly = "1" },
		"unknown-writable": func(d *deviceTopology) { d.readOnly = "" },
		"unknown-capacity": func(d *deviceTopology) { d.size = "invalid" },
		"too-small":        func(d *deviceTopology) { d.size = "1024" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := blank
			mutate(&fixture)
			if err := validateWholeDevice(fixture); err == nil {
				t.Fatal("unsafe or unknown topology accepted")
			}
		})
	}
}

func TestIndependentSignatureProbesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name              string
		wipeData, blkData string
		wipeCode, blkCode int
		wipeErr, blkErr   error
		wantOK            bool
		wantCalls         int
	}{
		{name: "blank", wipeData: `{"signatures":[]}`, blkCode: 2, wantOK: true, wantCalls: 2},
		{name: "existing-luks", wipeData: `{"signatures":[{"type":"crypto_LUKS"}]}`, wantCalls: 1},
		{name: "partition-table", wipeData: `{"signatures":[{"type":"gpt"}]}`, wantCalls: 1},
		{name: "missing-signatures", wipeData: `{}`, wantCalls: 1},
		{name: "null-signatures", wipeData: `{"signatures":null}`, wantCalls: 1},
		{name: "broken-json", wipeData: `{"signatures":`, wantCalls: 1},
		{name: "wipe-probe-failure", wipeCode: 1, wantCalls: 1},
		{name: "wipe-launch-failure", wipeErr: errors.New("failure"), wantCalls: 1},
		{name: "blkid-existing", wipeData: `{"signatures":[]}`, blkData: "TYPE=ext4\n", wantCalls: 2},
		{name: "blkid-ambiguous", wipeData: `{"signatures":[]}`, blkCode: 8, wantCalls: 2},
		{name: "blkid-incomplete", wipeData: `{"signatures":[]}`, blkData: "unexpected", blkCode: 2, wantCalls: 2},
		{name: "blkid-launch-failure", wipeData: `{"signatures":[]}`, blkErr: errors.New("failure"), wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			v := systemVolume{wipefs: "/usr/sbin/wipefs", blkid: "/usr/sbin/blkid"}
			v.runner = func(_ context.Context, binary string, args []string, _ *os.File, _ console, interactive bool) ([]byte, int, error) {
				if interactive || args[len(args)-1] != inheritedDevice {
					t.Fatalf("unsafe probe invocation: %s %v", binary, args)
				}
				calls = append(calls, binary)
				if binary == v.wipefs {
					if !reflect.DeepEqual(args, []string{"--json", "--no-act", inheritedDevice}) {
						t.Fatalf("unsafe signature probe arguments: %v", args)
					}
					return []byte(tc.wipeData), tc.wipeCode, tc.wipeErr
				}
				return []byte(tc.blkData), tc.blkCode, tc.blkErr
			}
			err := v.checkNoSignatures(context.Background())
			if (err == nil) != tc.wantOK || len(calls) != tc.wantCalls {
				t.Fatalf("probe result = %v, commands %v", err, calls)
			}
		})
	}
}

func TestResolvedMke2fsExplicitlyCreatesExt4(t *testing.T) {
	mapper, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer mapper.Close()
	v := systemVolume{mapper: mapper, mkfs: "/usr/sbin/mke2fs", blkid: "/usr/sbin/blkid"}
	var calls []string
	v.runner = func(_ context.Context, binary string, args []string, device *os.File, _ console, interactive bool) ([]byte, int, error) {
		if device != mapper || interactive {
			t.Fatal("filesystem command did not use the held verified mapper")
		}
		calls = append(calls, binary)
		if binary == v.mkfs {
			if !reflect.DeepEqual(args, []string{"-q", "-t", "ext4", "-L", "opsecmkt-state", inheritedDevice}) {
				t.Fatalf("resolved mke2fs lacks an explicit ext4 selection: %v", args)
			}
			return nil, 0, nil
		}
		return []byte("ext4\n"), 0, nil
	}
	if err := v.MakeFilesystem(context.Background()); err != nil || !reflect.DeepEqual(calls, []string{v.mkfs, v.blkid}) {
		t.Fatalf("filesystem creation/probe failed: %v, %v", err, calls)
	}
}

func TestMountedDeviceAndDestinationRefused(t *testing.T) {
	data := []byte("1 0 8:1 / / rw,relatime - ext4 /dev/vda1 rw\n2 1 8:2 / /srv/encrypted\\040state rw - ext4 /dev/vdb rw\n")
	if err := rejectMountedDevice(data, "8:3", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ device, mount string }{{"8:1", ""}, {"8:2", ""}, {"", "/srv/encrypted state"}} {
		if err := rejectMountedDevice(data, tc.device, tc.mount); err == nil {
			t.Fatalf("accepted mounted device/destination: %v", tc)
		}
	}
	for _, data := range [][]byte{nil, []byte("malformed"), []byte("1 2 not-a-device / / rw - ext4 /dev/vdb rw")} {
		if err := rejectMountedDevice(data, "8:3", ""); err == nil {
			t.Fatal("unreadable or malformed inventory accepted")
		}
	}
}

func TestUnknownSwapStateRefused(t *testing.T) {
	if err := rejectSwap([]byte("Filename\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"), 1); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "unknown", "Filename\tType Size Used Priority\n/dev/missing-swap partition 1024 0 -2", "Filename\tType Size Used Priority\nmalformed"} {
		if err := rejectSwap([]byte(data), 1); err == nil {
			t.Fatal("unknown swap state accepted")
		}
	}
}

func TestCommandOutputBoundAndEnvironmentIsolation(t *testing.T) {
	var output boundedOutput
	if _, err := output.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("x")); err == nil {
		t.Fatal("accepted unbounded native output")
	}
	t.Setenv("OPSECMKT_TEST_SECRET_NO_FORWARD", "never-forward-this")
	data, code, err := runCommand(context.Background(), "/usr/bin/env", nil, nil, console{}, false)
	if err != nil || code != 0 || strings.Contains(string(data), "never-forward-this") || strings.Contains(string(data), "OPSECMKT_TEST_SECRET") {
		t.Fatalf("command inherited caller environment or failed: code %d, error %v", code, err)
	}
}

func TestLocalBlockOperationsRefuseNonTerminal(t *testing.T) {
	_, err := openSystemVolume(context.Background(), options{}, console{})
	if err == nil {
		t.Fatal("nonterminal block operation accepted")
	}
}

func TestLateNativeFailureCleansOnlyProvenMapping(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	v := systemVolume{cfg: options{name: "only-the-new-mapping"}, cryptsetup: "/usr/sbin/cryptsetup"}
	v.runner = func(_ context.Context, binary string, args []string, _ *os.File, _ console, interactive bool) ([]byte, int, error) {
		if binary != v.cryptsetup || interactive || !reflect.DeepEqual(args, []string{"close", v.cfg.name}) {
			t.Fatalf("cleanup attempted an unrelated operation: %s %v", binary, args)
		}
		calls = append(calls, args[0])
		return nil, 0, nil
	}
	// Native open may have failed to report success after registering this
	// proven mapping. It must still become eligible for deferred cleanup.
	v.recordMapping(file, nil, true)
	if err := v.Cleanup(context.Background()); err != nil || len(calls) != 1 || v.opened || v.mapper != nil {
		t.Fatalf("late failure left a proven mapping open: %v, %v", err, calls)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("cleanup did not release mapping descriptor")
	}

	calls = nil
	v.recordMapping(nil, errors.New("different mapping identity"), true)
	if err := v.Cleanup(context.Background()); err == nil || len(calls) != 0 {
		t.Fatalf("unknown mapping must be preserved and reported: %v, %v", err, calls)
	}
}

func TestCleanAbsolutePaths(t *testing.T) {
	for _, path := range []string{"/srv/state", "/dev/disk/by-id/new-data"} {
		if !cleanAbsolute(path) {
			t.Fatalf("clean absolute path refused: %q", path)
		}
	}
	for _, path := range []string{"relative", "/srv/../dev", "/srv//state", "/srv/state/", "/srv/line\nbreak"} {
		if cleanAbsolute(path) {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
}
