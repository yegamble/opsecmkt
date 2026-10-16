//go:build linux

package operator

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type bootFileInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i bootFileInfo) Name() string       { return "operator.json" }
func (i bootFileInfo) Size() int64        { return 10 }
func (i bootFileInfo) Mode() os.FileMode  { return i.mode }
func (i bootFileInfo) ModTime() time.Time { return time.Time{} }
func (i bootFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i bootFileInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestBootGuardRefusesNonrootMutableOrSymlinkedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name      string
		info      bootFileInfo
		directory bool
		allowed   bool
	}{
		{"root private file", bootFileInfo{0600, 0}, false, true},
		{"root private directory", bootFileInfo{os.ModeDir | 0700, 0}, true, true},
		{"nonroot owner", bootFileInfo{0600, 1000}, false, false},
		{"group writable", bootFileInfo{0660, 0}, false, false},
		{"other writable", bootFileInfo{0602, 0}, false, false},
		{"symlink", bootFileInfo{os.ModeSymlink | 0600, 0}, false, false},
		{"directory instead of file", bootFileInfo{os.ModeDir | 0700, 0}, false, false},
		{"file instead of directory", bootFileInfo{0600, 0}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := trustedRootInfo(tc.info, tc.directory); (err == nil) != tc.allowed {
				t.Fatalf("unexpected ownership decision: %v", err)
			}
		})
	}
}
