package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractiveDispatchPreservesHeadlessAndFlaggedCommands(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		terminal, want bool
	}{
		{nil, true, true}, {nil, false, false},
		{[]string{"deploy", "init"}, true, true}, {[]string{"init"}, true, true},
		{[]string{"serve"}, true, false}, {[]string{"deploy", "init", "--mode", "dev"}, true, false},
		{[]string{"deploy", "init"}, false, false}, {[]string{"--help"}, true, false},
		{[]string{"emergency", "stop"}, true, false},
	} {
		if got := shouldStartInteractive(tc.args, tc.terminal); got != tc.want {
			t.Fatalf("args=%v terminal=%v got=%v", tc.args, tc.terminal, got)
		}
	}
	var out bytes.Buffer
	handled, err := command([]string{"setup", "--help"}, strings.NewReader(""), &out, &out)
	if !handled || err != nil || !strings.Contains(out.String(), "Guided configuration") {
		t.Fatalf("setup help: %v %v %s", handled, err, out.String())
	}
	if terminalPair(strings.NewReader(""), &out) {
		t.Fatal("treated pipes as terminal")
	}
}
