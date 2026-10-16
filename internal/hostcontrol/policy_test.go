package hostcontrol

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestUnlockPolicyAcknowledgementIsBoundToCurrentBoot(t *testing.T) {
	for _, tc := range []struct {
		name, savedBoot, currentBoot  string
		enabled, allowed, bootChanged bool
	}{
		{name: "initial disabled", currentBoot: testBoot, allowed: true},
		{name: "current acknowledgement", savedBoot: testBoot, currentBoot: testBoot, enabled: true, allowed: true},
		{name: "after reboot", savedBoot: testBoot, currentBoot: nextBoot, enabled: true, bootChanged: true},
		{name: "copied true policy", currentBoot: testBoot, enabled: true, bootChanged: true},
		{name: "unknown boot", savedBoot: testBoot, currentBoot: "invalid", enabled: true, bootChanged: true},
		{name: "invalid saved boot", savedBoot: "invalid", currentBoot: testBoot, enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(UnlockPolicy{Version: 1, DeploymentDir: "/srv/preserved", UnlockRehearsed: tc.enabled, BootID: tc.savedBoot})
			policy, err := decodeUnlockPolicy(data, "/srv/preserved", tc.currentBoot)
			if (err == nil) != tc.allowed || errors.Is(err, errUnlockBootChanged) != tc.bootChanged {
				t.Fatalf("unexpected readiness decision: %#v %v", policy, err)
			}
			if tc.bootChanged && !policy.UnlockRehearsed {
				t.Fatal("stale true policy became permission to recreate missing ledger")
			}
		})
	}
}

func TestPolicyRejectsRepeatedUnknownAndWrongDeploymentFields(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"version":1,"deployment_dir":"/srv/preserved","unlock_rehearsed":false}`,
		`{"version":1,"deployment_dir":"/srv/other","unlock_rehearsed":true}`,
		`{"version":1,"deployment_dir":"/srv/preserved","unlock_rehearsed":true,"command":"reboot"}`,
	} {
		if _, err := decodeUnlockPolicy([]byte(data), "/srv/preserved", testBoot); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
