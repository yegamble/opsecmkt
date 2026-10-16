//go:build !linux

package emergency

import (
	"context"
	"errors"
)

type systemHost struct{}

func (*systemHost) Close() error { return nil }

func (*systemHost) checkDevice(context.Context, string) error {
	return errors.New("emergency: LUKS device operations are supported only on Linux")
}
func (*systemHost) erase(context.Context, string) error {
	return errors.New("emergency: LUKS device operations are supported only on Linux")
}
func (*systemHost) reboot() error {
	return errors.New("emergency: immediate reboot is supported only on Linux")
}
