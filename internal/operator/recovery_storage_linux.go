//go:build linux

package operator

import (
	"errors"

	"golang.org/x/sys/unix"
)

func checkRecoveryPersistent(path string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil || st.Type == unix.TMPFS_MAGIC || st.Type == unix.RAMFS_MAGIC {
		return errors.New("recovery restore: destination must use persistent storage, not tmpfs or ramfs")
	}
	return nil
}

func publishRecovery(stage, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
