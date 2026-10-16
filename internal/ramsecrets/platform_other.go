//go:build !linux

package ramsecrets

import "errors"

// IsTmpfs is false on non-Linux platforms so offline configuration tooling works.
// Hardened runtime verification itself remains Linux-only.
func IsTmpfs(string) (bool, error) { return false, nil }

// Verify is available only on Linux with statfs and /proc/self/mountinfo.
func Verify(string) error                { return errors.New("RAM secret verification requires Linux") }
func mountMemory(string) error           { return errors.New("RAM secret mounting requires Linux") }
func publishMemory(string, string) error { return errors.New("RAM secret publication requires Linux") }
