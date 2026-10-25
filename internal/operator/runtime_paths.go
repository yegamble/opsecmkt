package operator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"opsecmkt/internal/ramsecrets"
)

var operatorIsTmpfs = ramsecrets.IsTmpfs

func checkRetentionStorage(dir string, c Config) error {
	ram, err := operatorIsTmpfs(dir)
	if err != nil {
		return errors.New("cannot verify installation storage type")
	}
	if !ram {
		return nil
	}
	relative, err := filepath.Rel(dir, c.RetentionDir)
	if err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("RAM installation requires an explicit external persistent --retention-dir; retention state cannot live inside the RAM installation")
	}
	ram, err = operatorIsTmpfs(c.RetentionDir)
	if err != nil || ram {
		return errors.New("RAM installation requires retention state on a verified persistent filesystem outside tmpfs")
	}
	return nil
}
func prepareAdminSocket(c Config) error {
	err := os.Mkdir(c.AdminSocketDir, 0750)
	created := err == nil
	if err != nil && !os.IsExist(err) {
		return err
	}
	before, err := os.Lstat(c.AdminSocketDir)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || !created && before.Mode().Perm() != 0750 {
		return errors.New("administrator socket directory must be a real private mode-0750 directory")
	}
	dir, err := os.Open(c.AdminSocketDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		return errors.New("administrator socket directory changed")
	}
	if created {
		// Mkdir's mode is reduced by the operator's umask. Set the exact
		// contract only on a directory this invocation just created; never
		// repair a pre-existing directory with unexpected permissions.
		if err := dir.Chmod(0750); err != nil {
			return err
		}
	}
	if os.Geteuid() == 0 {
		if err := dir.Chown(c.RuntimeUID, c.RuntimeGID); err != nil {
			return errors.New("cannot assign private administrator socket ownership")
		}
	}
	return nil
}
