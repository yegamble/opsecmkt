//go:build linux

package ramsecrets

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// IsTmpfs checks Linux filesystem type, walking to the nearest existing parent
// when path does not yet exist. It does not classify other volatile filesystems
// as persistent or prove that snapshots/backing storage are safe.
func IsTmpfs(path string) (bool, error) {
	if path == "" {
		return false, errors.New("filesystem path required")
	}
	current, err := filepath.Abs(path)
	if err != nil {
		return false, errors.New("invalid filesystem path")
	}
	for {
		if _, err := os.Stat(current); err == nil {
			var stat unix.Statfs_t
			if err := unix.Statfs(current, &stat); err != nil {
				return false, errors.New("cannot inspect filesystem type")
			}
			return stat.Type == unix.TMPFS_MAGIC, nil
		} else if !os.IsNotExist(err) {
			return false, errors.New("cannot inspect filesystem path")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, errors.New("no existing filesystem parent")
		}
		current = parent
	}
}

// Verify requires a real root-owned mode0700 directory on tmpfs with nosuid,
// nodev and noexec observed both through statfs and the containing mount record.
// It does not guarantee absence from swap, core dumps, snapshots or live memory.
func Verify(path string) error { return verifyMemory(path, false) }

func verifyMemory(path string, exact bool) error {
	if err := realDirectoryPath(path); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil || st.Uid != 0 || st.Mode&07777 != 0700 {
		return errors.New("RAM directory must be root-owned with mode0700")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil || stat.Type != unix.TMPFS_MAGIC {
		return errors.New("RAM directory must use actual tmpfs")
	}
	required := int64(unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC)
	if int64(stat.Flags)&required != required {
		return errors.New("tmpfs must enforce nosuid,nodev,noexec")
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return errors.New("cannot verify actual mount flags")
	}
	defer f.Close()
	info, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(info) > 2<<20 {
		return errors.New("mount information unavailable or oversized")
	}
	if err := validateMountInfo(path, info, exact); err != nil {
		return errors.New("actual containing mount must be hardened tmpfs")
	}
	return nil
}

func publishMemory(stage, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}

func realDirectoryPath(path string) error {
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("RAM directory must be a clean absolute nonroot path")
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("RAM directory path cannot contain symlinks or non-directories")
		}
	}
	return nil
}

func mountMemory(path string) error {
	if os.Geteuid() != 0 {
		return errors.New("secrets mount: Linux root privileges are required")
	}
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("secrets mount: target must be a clean absolute nonroot path")
	}
	if err := realDirectoryPath(filepath.Dir(path)); err != nil {
		return errors.New("secrets mount: parent must exist without symlinks")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("secrets mount: existing target is not a real directory")
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil || len(entries) != 0 {
			return errors.New("secrets mount: existing target must be empty")
		}
		if err := verifyMemory(path, true); err != nil {
			return errors.New("secrets mount: existing target is not an exact hardened tmpfs mount")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("secrets mount: target cannot be inspected")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return errors.New("secrets mount: cannot create fresh mount target")
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("tmpfs", path, "tmpfs", flags, "mode=0700,size=16m"); err != nil {
		_ = os.Remove(path)
		return errors.New("secrets mount: tmpfs mount failed; root mount capability is required")
	}
	if err := verifyMemory(path, true); err != nil {
		// This invocation created the empty mount; no unlocked files/services exist.
		if unix.Unmount(path, 0) == nil {
			_ = os.Remove(path)
		}
		return errors.New("secrets mount: newly mounted filesystem did not pass verification")
	}
	return nil
}
