//go:build !linux

package operator

import (
	"context"
	"errors"
)

func ensureProductionBootGuard(context.Context, string, Config) error {
	return errors.New("production boot storage guards require Linux/systemd; run deployment on the actual host")
}

func verifyBootConfiguration(string) error {
	return errors.New("production boot configuration requires a root-owned Linux installation")
}
