//go:build linux

package hostbootstrap

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestSafeReadMissingAncestorAndUnsafeParent(t *testing.T) {
	info, err := os.Stat("/etc")
	if err != nil {
		t.Fatal(err)
	}
	if info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Skip("managed workspace remaps root ownership; also run this test in the Ubuntu container")
	}
	s, e := safeRead("/proc/opsecmkt-test-parent-does-not-exist/dropin.conf", 1<<20)
	if e != nil || s.Exists {
		t.Fatalf("missing ancestor: %+v %v", s, e)
	}
	if _, e = safeRead("/tmp/untrusted-parent/opsecmkt.conf", 1<<20); e == nil {
		t.Fatal("accepted world-writable ancestor")
	}
	if _, e = safeRead("/var/run/opsecmkt-test.conf", 1<<20); e == nil {
		t.Fatal("accepted symlink ancestor")
	}
}
func TestPendingStateBoundBeforeMutation(t *testing.T) {
	p := pending{Files: map[string]savedFile{"one": {Exists: true, Data: make([]byte, 800<<10)}}}
	if _, e := marshalPending(p); e == nil {
		t.Fatal("accepted oversized base64 snapshot")
	}
	p.Files = map[string]savedFile{}
	for _, name := range []string{"a", "b", "c", "d"} {
		p.Files[name] = savedFile{Exists: true, Data: make([]byte, 200<<10)}
	}
	if _, e := marshalPending(p); e == nil {
		t.Fatal("accepted oversized aggregate snapshot")
	}
	p.Files = map[string]savedFile{"small": {Exists: true, Data: make([]byte, 32<<10)}}
	if _, e := marshalPending(p); e != nil {
		t.Fatal(e)
	}
}
func TestUnlistedBridgeCannotMasqueradeAsDocker(t *testing.T) {
	c := fixture()
	inventory := `[{"ifname":"eth0","addr_info":[]},{"ifname":"eth1","addr_info":[{"local":"10.20.0.10"}]},{"ifname":"br-public","linkinfo":{"info_kind":"bridge"}}]`
	if validateInterfaces(c, []byte(inventory), map[string]bool{}) == nil {
		t.Fatal("accepted arbitrary bridge prefix")
	}
	inventory = strings.ReplaceAll(inventory, "br-public", "docker0")
	if validateInterfaces(c, []byte(inventory), map[string]bool{"docker0": true}) != nil {
		t.Fatal("refused proven Docker bridge")
	}
	inventory = strings.ReplaceAll(inventory, `"info_kind":"bridge"`, `"info_kind":"dummy"`)
	if validateInterfaces(c, []byte(inventory), map[string]bool{"docker0": true}) == nil {
		t.Fatal("name is insufficient without bridge type")
	}
}
func TestFirewallBootMustFailWhenPolicyMissing(t *testing.T) {
	if strings.Contains(firewallService, "ConditionPathExists") {
		t.Fatal("missing policy would skip instead of fail")
	}
	for _, s := range []string{"Requires=" + rollbackUnit, "Before=network-pre.target docker.service containerd.service ssh.service ssh.socket", "host-load-firewall"} {
		if !strings.Contains(firewallService, s) {
			t.Fatalf("missing boot dependency %s", s)
		}
	}
	for _, name := range []string{"/etc/systemd/system/ssh.service.d/10-opsecmkt-host.conf", "/etc/systemd/system/ssh.socket.d/10-opsecmkt-host.conf"} {
		found := false
		for _, path := range controlledFiles {
			if path == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("SSH failure dependency not managed: %s", name)
		}
	}
	for _, data := range rollbackFiles() {
		if strings.Contains(data, "host rollback") && !strings.Contains(data, "Restart=on-failure") {
			t.Fatal("rollback contention must retry")
		}
	}
}
func TestLiveRulesHashIgnoresCountersButDetectsPermissionDrift(t *testing.T) {
	a := []byte(`{"nftables":[{"rule":{"handle":3,"expr":[{"counter":{"packets":2,"bytes":64}},{"drop":null}]}}]}`)
	b := []byte(`{"nftables":[{"rule":{"handle":8,"expr":[{"counter":{"packets":7,"bytes":400}},{"drop":null}]}}]}`)
	ah, e := normalizedRulesHash(a)
	if e != nil {
		t.Fatal(e)
	}
	bh, e := normalizedRulesHash(b)
	if e != nil || ah != bh {
		t.Fatal("normal traffic changed fingerprint")
	}
	ch, e := normalizedRulesHash([]byte(strings.ReplaceAll(string(b), "drop", "accept")))
	if e != nil || ah == ch {
		t.Fatal("permission drift not detected")
	}
}
