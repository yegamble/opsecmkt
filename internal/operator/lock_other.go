//go:build !linux && !darwin

package operator

import "errors"

func lockInstallation(string) (func(), error) {
	return nil, errors.New("deployment mutation is supported on Linux and macOS only")
}
func syncDirectory(string) error { return nil }
