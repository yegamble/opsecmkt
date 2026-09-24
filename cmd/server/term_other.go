//go:build !linux

package main

import (
	"errors"
	"os"
)

// echoOff cannot hide typed input outside Linux (the release image is Linux), so there a character device
// such as a terminal is refused and the new password must be piped in; anything else is read as a pipe.
func echoOff(f *os.File) (func(), error) {
	if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return nil, errors.New("cannot hide typed input on this system; pipe the new password on stdin")
	}
	return nil, errNotTerminal
}
