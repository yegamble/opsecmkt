//go:build !linux

package operator

import (
	"context"
	"errors"
)

func ensureHostRebootService(context.Context, string, Config) error {
	return errors.New("private host reboot service requires the actual Linux/systemd host")
}
func writeHostRebootReadiness(string, Config) error {
	return errors.New("unlock-rehearsal acknowledgement requires the actual Linux host")
}
