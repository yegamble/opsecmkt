package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func isInteractiveTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
