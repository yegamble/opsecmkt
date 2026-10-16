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
	if err := os.Mkdir(c.AdminSocketDir, 0750); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(c.AdminSocketDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0750 {
		return errors.New("administrator socket directory must be a real private mode-0750 directory")
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(c.AdminSocketDir, c.RuntimeUID, c.RuntimeGID); err != nil {
			return errors.New("cannot assign private administrator socket ownership")
		}
	}
	return nil
}
