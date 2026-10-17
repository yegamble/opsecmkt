//go:build !linux

package hostcontrol

import (
	"context"
	"io"
	"net"
	"testing"
)

func TestUnsupportedHostAndPeerProofFailClosed(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if verifyRootPeer(left) == nil {
		t.Fatal("unsupported host supplied root peer proof")
	}
	if runPlatformAgent(context.Background(), agentConfig{}, io.Discard, io.Discard, nil) == nil {
		t.Fatal("unsupported host started a control agent")
	}
}
