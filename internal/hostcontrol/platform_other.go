//go:build !linux

package hostcontrol

import (
	"context"
	"errors"
	"io"
	"net"
)

func verifyRootPeer(net.Conn) error {
	return errors.New("host control requires Linux peer credentials")
}

func runPlatformAgent(context.Context, agentConfig, io.Writer, io.Writer, func(context.Context, string) error) error {
	return errors.New("host control agent requires a supported Linux root host")
}
