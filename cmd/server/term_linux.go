package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// echoOff turns off terminal echo on f (keeping line editing and Ctrl-C) and returns a function restoring the
// previous settings. It returns errNotTerminal when f is not a terminal.
func echoOff(f *os.File) (func(), error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errNotTerminal
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	if err = unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return nil, err
	}
	return func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }, nil
}
