//go:build !linux

package storageguard

import "errors"

func platformInspect(string) (Evidence, error) {
	return Evidence{}, errors.New("encrypted production storage verification requires Linux on the actual service host")
}
