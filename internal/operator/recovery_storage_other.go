//go:build !linux

package operator

import (
	"errors"
	"os"
	"path/filepath"
)

func checkRecoveryPersistent(string) error {
	return errors.New("recovery restore: persistent filesystem verification requires Linux")
}

// Export remains available off Linux. Claim the directory exclusively before
// publishing our ciphertext; persistent plaintext restore is Linux-only.
func publishRecovery(stage, target string) error {
	if err := os.Mkdir(target, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
