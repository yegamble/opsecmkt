package storageguard

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureUUID = "00112233-4455-6677-8899-aabbccddeeff"
const secondUUID = "11223344-5566-7788-99aa-bbccddeeff00"
const fixtureFilesystemUUID = "22334455-6677-8899-aabb-ccddeeff0011"

type fixture struct {
	t      *testing.T
	root   string
	state  string
	sys    string
	mounts string
	device string
	nodes  map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, state: filepath.Join(root, "state"), sys: filepath.Join(root, "sys"), mounts: filepath.Join(root, "mountinfo"), device: "253:0", nodes: map[string]string{}}
	f.mkdir(f.state)
	f.mkdir(filepath.Join(f.sys, "dev", "block"))
	f.mount("253:0", "ext4", f.state)
	f.node("8:1", "disk1", "")
	f.node("253:0", "dm-0", "")
	f.slaves("253:0", "8:1")
	f.luks("253:0", fixtureUUID)
	return f
}

func (f *fixture) mkdir(path string) {
	f.t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write(path, value string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func escapeMount(path string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(path)
}

func (f *fixture) mount(device, filesystem, path string) {
	f.t.Helper()
	f.write(f.mounts, fmt.Sprintf("1 0 8:0 / / rw - ext4 /dev/root rw\n2 1 %s / %s rw,nosuid,nodev - %s /dev/mapper/test rw\n", device, escapeMount(path), filesystem))
}

func (f *fixture) node(device, name, parent string) {
	f.t.Helper()
	path := filepath.Join(f.sys, "devices", "virtual", "block", name)
	if parent != "" {
		path = filepath.Join(f.nodes[parent], name)
	}
	f.mkdir(filepath.Join(path, "slaves"))
	f.write(filepath.Join(path, "dev"), device+"\n")
	if parent != "" {
		f.write(filepath.Join(path, "partition"), "1\n")
	}
	if err := os.Symlink(path, filepath.Join(f.sys, "dev", "block", device)); err != nil {
		f.t.Fatal(err)
	}
	f.nodes[device] = path
}

func (f *fixture) slaves(device string, children ...string) {
	f.t.Helper()
	dir := filepath.Join(f.nodes[device], "slaves")
	for i, child := range children {
		if err := os.Symlink(f.nodes[child], filepath.Join(dir, fmt.Sprintf("child%d", i))); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) luks(device, uuid string) {
	f.t.Helper()
	f.write(filepath.Join(f.nodes[device], "dm", "uuid"), "CRYPT-LUKS2-"+strings.ReplaceAll(uuid, "-", "")+"-state\n")
	f.write(filepath.Join(f.nodes[device], "dm", "suspended"), "0\n")
}

func (f *fixture) inspect(path string) (Evidence, error) {
	f.t.Helper()
	return inspect(path, inspector{mountInfo: f.mounts, sysRoot: f.sys, device: func(string) (string, error) { return f.device, nil }, cryptStatus: func(string, string) error { return nil }, filesystemUUID: func(string, string) (string, error) { return fixtureFilesystemUUID, nil }})
}

func TestInspectActiveLUKS2AndFuturePathWithoutWrites(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.state, "new", "deployment")
	before, err := os.ReadDir(f.state)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := f.inspect(target)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Path != target || evidence.ExistingPath != f.state || evidence.MountPoint != f.state || evidence.MountRoot != "/" || evidence.FilesystemUUID != fixtureFilesystemUUID || evidence.Device != "253:0" || evidence.Filesystem != "ext4" || !reflect.DeepEqual(evidence.LUKSUUIDs, []string{fixtureUUID}) {
		t.Fatalf("unexpected storage proof: %#v", evidence)
	}
	after, err := os.ReadDir(f.state)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("inspection wrote into the target")
	}
	if _, err := os.Lstat(filepath.Join(f.state, "new")); !os.IsNotExist(err) {
		t.Fatal("inspection created future directories")
	}
}

func TestInspectDoesNotReadPrivateFileContents(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.state, "private-input")
	const private = "secret-value-that-must-not-be-read-or-reported"
	f.write(path, private)
	evidence, err := f.inspect(path)
	if err != nil || evidence.ExistingPath != path || strings.Contains(fmt.Sprint(evidence, err), private) {
		t.Fatalf("private path inspection: %#v %v", evidence, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != private {
		t.Fatal("private input changed")
	}
}

func TestInspectRejectsUnencryptedVolatileRemoteAndUnknownFilesystems(t *testing.T) {
	for _, filesystem := range []string{"tmpfs", "overlay", "nfs4", "cifs", "btrfs", "zfs", "unknown"} {
		t.Run(filesystem, func(t *testing.T) {
			f := newFixture(t)
			f.mount("253:0", filesystem, f.state)
			if _, err := f.inspect(f.state); err == nil {
				t.Fatal("unsupported filesystem accepted")
			}
		})
	}
	f := newFixture(t)
	f.device = "8:1"
	f.mount(f.device, "ext4", f.state)
	if _, err := f.inspect(f.state); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("plain persistent disk accepted: %v", err)
	}
}

func TestInspectLVMAndPartitionAboveEncryption(t *testing.T) {
	for _, kind := range []string{"LVM", "partition"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if kind == "LVM" {
				f.node("253:1", "dm-1", "")
				f.write(filepath.Join(f.nodes["253:1"], "dm", "uuid"), "LVM-volume-identity\n")
				f.slaves("253:1", "253:0")
			} else {
				f.node("253:1", "dm-0p1", "253:0")
			}
			f.device = "253:1"
			f.mount(f.device, "xfs", f.state)
			proof, err := f.inspect(f.state)
			if err != nil || !reflect.DeepEqual(proof.LUKSUUIDs, []string{fixtureUUID}) {
				t.Fatalf("protected %s ancestry rejected: %#v %v", kind, proof, err)
			}
		})
	}
}

func TestInspectRequiresEveryMDBackingBranchEncrypted(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprintf("plain=%v", plain), func(t *testing.T) {
			f := newFixture(t)
			f.node("8:2", "disk2", "")
			f.node("253:1", "dm-1", "")
			f.slaves("253:1", "8:2")
			f.luks("253:1", secondUUID)
			f.node("9:0", "md0", "")
			other := "253:1"
			if plain {
				other = "8:2"
			}
			f.slaves("9:0", "253:0", other)
			f.device = "9:0"
			f.mount(f.device, "ext4", f.state)
			proof, err := f.inspect(f.state)
			if plain {
				if err == nil {
					t.Fatal("mixed encrypted/plain branches accepted")
				}
			} else if err != nil || !reflect.DeepEqual(proof.LUKSUUIDs, []string{fixtureUUID, secondUUID}) {
				t.Fatalf("all encrypted branches rejected: %#v %v", proof, err)
			}
		})
	}
}

func TestInspectNestedPlainMountOverridesEncryptedParent(t *testing.T) {
	f := newFixture(t)
	nested := filepath.Join(f.state, "nested")
	f.mkdir(nested)
	data, err := os.ReadFile(f.mounts)
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.mounts, string(data)+fmt.Sprintf("3 2 8:1 / %s rw - ext4 /dev/plain rw\n", escapeMount(nested)))
	f.device = "8:1"
	if _, err := f.inspect(filepath.Join(nested, "new")); err == nil {
		t.Fatal("encrypted ancestor concealed nested plain mount")
	}
	// A stale parent device must not let a mismatched nested mount pass either.
	f.device = "253:0"
	if _, err := f.inspect(nested); err == nil {
		t.Fatal("containing mount/device mismatch accepted")
	}
}

func TestInspectRejectsSymlinkAncestorsAndUncleanPaths(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(f.state, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative", f.state + "/../state", filepath.Join(link, "new"), link, f.state + "\nprivate"} {
		if _, err := f.inspect(path); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	f.write(filepath.Join(f.state, "file"), "value")
	if _, err := f.inspect(filepath.Join(f.state, "file", "child")); err == nil {
		t.Fatal("regular file accepted as future directory ancestor")
	}
}

func TestInspectRejectsInactiveMalformedAndUnverifiableMappings(t *testing.T) {
	for _, kind := range []string{"suspended", "missing-status", "LUKS1", "plain-crypt", "malformed-uuid", "missing-backing", "unknown-device", "oversized-metadata", "identity-mismatch", "escaped-sysfs"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			dm := filepath.Join(f.nodes["253:0"], "dm")
			switch kind {
			case "suspended":
				f.write(filepath.Join(dm, "suspended"), "1\n")
			case "missing-status":
				if err := os.Remove(filepath.Join(dm, "suspended")); err != nil {
					t.Fatal(err)
				}
			case "LUKS1":
				f.write(filepath.Join(dm, "uuid"), "CRYPT-LUKS1-00112233445566778899aabbccddeeff-state\n")
			case "plain-crypt":
				f.write(filepath.Join(dm, "uuid"), "CRYPT-PLAIN-state\n")
			case "malformed-uuid":
				f.write(filepath.Join(dm, "uuid"), "CRYPT-LUKS2-not-a-uuid-state\n")
			case "missing-backing":
				if err := os.Remove(filepath.Join(f.nodes["253:0"], "slaves", "child0")); err != nil {
					t.Fatal(err)
				}
			case "unknown-device":
				f.device = "253:99"
				f.mount(f.device, "ext4", f.state)
			case "oversized-metadata":
				f.write(filepath.Join(dm, "uuid"), strings.Repeat("X", 257))
			case "identity-mismatch":
				f.write(filepath.Join(f.nodes["253:0"], "dev"), "253:1\n")
			case "escaped-sysfs":
				outside := filepath.Join(f.root, "outside")
				f.mkdir(outside)
				f.write(filepath.Join(outside, "dev"), "253:0\n")
				link := filepath.Join(f.sys, "dev", "block", "253:0")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.inspect(f.state); err == nil {
				t.Fatal("unverifiable mapping accepted")
			}
		})
	}
}

func TestInspectRejectsCyclicDeviceGraph(t *testing.T) {
	f := newFixture(t)
	f.node("253:2", "dm-2", "")
	f.node("253:3", "dm-3", "")
	f.slaves("253:2", "253:3")
	f.slaves("253:3", "253:2")
	f.device = "253:2"
	f.mount(f.device, "ext4", f.state)
	if _, err := f.inspect(f.state); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cyclic graph accepted: %v", err)
	}
}

func TestInspectEscapedMountPaths(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.state, "space and\\backslash")
	f.mkdir(path)
	f.mount(f.device, "ext4", path)
	proof, err := f.inspect(path)
	if err != nil || proof.MountPoint != path {
		t.Fatalf("escaped mount path rejected: %#v %v", proof, err)
	}
	for _, value := range []string{`\`, `\03`, `\999`} {
		if _, err := unescapeMountPath(value); err == nil {
			t.Fatalf("malformed escape accepted: %q", value)
		}
	}
}

func TestInspectDetectsDeviceChangeDuringVerification(t *testing.T) {
	f := newFixture(t)
	calls := 0
	_, err := inspect(f.state, inspector{mountInfo: f.mounts, sysRoot: f.sys, cryptStatus: func(string, string) error { return nil }, filesystemUUID: func(string, string) (string, error) { return fixtureFilesystemUUID, nil }, device: func(string) (string, error) {
		calls++
		if calls == 1 {
			return "253:0", nil
		}
		return "8:1", nil
	}})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mount replacement accepted: %v", err)
	}
}

func TestInspectRequiresActualCryptTargetProof(t *testing.T) {
	f := newFixture(t)
	for _, status := range []func(string, string) error{nil, func(string, string) error { return fmt.Errorf("linear target with forged LUKS2 label") }} {
		_, err := inspect(f.state, inspector{mountInfo: f.mounts, sysRoot: f.sys, device: func(string) (string, error) { return f.device, nil }, cryptStatus: status})
		if err == nil || !strings.Contains(err.Error(), "crypt target") {
			t.Fatalf("UUID metadata alone accepted: %v", err)
		}
	}
}

func TestInspectReportsBindMountRootAndActualFilesystemUUID(t *testing.T) {
	f := newFixture(t)
	data, err := os.ReadFile(f.mounts)
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.mounts, strings.Replace(string(data), "253:0 / ", "253:0 /private\\040state ", 1))
	evidence, err := f.inspect(f.state)
	if err != nil || evidence.MountRoot != "/private state" || evidence.FilesystemUUID != fixtureFilesystemUUID {
		t.Fatalf("bind mount identity omitted: %#v %v", evidence, err)
	}
	// Two LVs/filesystems can share an outer LUKS mapping. Return their actual
	// filesystem identity, so callers can pin more than that common boundary.
	other, err := inspect(f.state, inspector{mountInfo: f.mounts, sysRoot: f.sys, device: func(string) (string, error) { return f.device, nil }, cryptStatus: func(string, string) error { return nil }, filesystemUUID: func(string, string) (string, error) { return secondUUID, nil }})
	if err != nil || !reflect.DeepEqual(other.LUKSUUIDs, evidence.LUKSUUIDs) || other.FilesystemUUID == evidence.FilesystemUUID {
		t.Fatalf("filesystem below shared encryption was not distinguished: %#v %v", other, err)
	}
}

func TestInspectRefusesMissingOrInvalidFilesystemUUID(t *testing.T) {
	f := newFixture(t)
	for _, uuid := range []string{"", "not-a-uuid", strings.ToUpper(fixtureFilesystemUUID), fixtureFilesystemUUID + "\n" + secondUUID} {
		_, err := inspect(f.state, inspector{mountInfo: f.mounts, sysRoot: f.sys, device: func(string) (string, error) { return f.device, nil }, cryptStatus: func(string, string) error { return nil }, filesystemUUID: func(string, string) (string, error) { return uuid, nil }})
		if err == nil || !strings.Contains(err.Error(), "filesystem UUID") {
			t.Fatalf("unverifiable filesystem UUID accepted: %q %v", uuid, err)
		}
	}
	_, err := inspect(f.state, inspector{mountInfo: f.mounts, sysRoot: f.sys, device: func(string) (string, error) { return f.device, nil }, cryptStatus: func(string, string) error { return nil }})
	if err == nil {
		t.Fatal("missing filesystem identity probe accepted")
	}
}

func TestCanonicalMappingAndDeviceIdentities(t *testing.T) {
	for _, value := range []string{"", "0", "1:2:3", "-1:2", "01:2", "1:02", "4294967296:0"} {
		if validDevice(value) {
			t.Fatalf("invalid device accepted: %q", value)
		}
	}
	if !validDevice("0:0") || !validDevice("253:123") {
		t.Fatal("canonical block device rejected")
	}
	for _, value := range []string{"", "CRYPT-LUKS2-00112233445566778899aabbccddeeff-", "CRYPT-LUKS2-00112233445566778899aabbccddeefg-state", "CRYPT-LUKS2-00112233-4455-6677-8899-aabbccddeeff-state"} {
		if _, ok := luks2UUID(value); ok {
			t.Fatalf("invalid mapping UUID accepted: %q", value)
		}
	}
}
