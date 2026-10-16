package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func isInteractiveTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
