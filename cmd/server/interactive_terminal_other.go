//go:build !linux && !darwin

package main

import "os"

func isInteractiveTerminal(*os.File) bool { return false }
