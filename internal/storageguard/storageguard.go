// Package storageguard verifies guest-visible persistent encryption before
// sensitive production state is written. It does not attest a hosting provider,
// an unlocked machine, or the confidentiality of memory, swap, or old backups.
package storageguard

// Evidence describes a successfully verified filesystem. LUKSUUIDs contains
// sorted canonical UUIDs of the active LUKS2 mappings protecting every backing
// branch. Path may not exist yet; ExistingPath is its nearest existing ancestor.
type Evidence struct {
	Path           string
	ExistingPath   string
	MountPoint     string
	MountRoot      string
	Device         string
	Filesystem     string
	FilesystemUUID string
	LUKSUUIDs      []string
}

// Inspect requires an ext4 or XFS filesystem whose actual block-device ancestry
// is protected on every branch by a guest-visible active LUKS2 mapping. Unknown,
// network, volatile, overlay, and multi-device Btrfs filesystems fail closed.
// Linux requires root and a trusted installed cryptsetup binary to verify the
// actual active crypt target without reading or printing its encryption key.
// Every existing component of path must be real, without symlinks. Inspection
// is read-only; it never mounts, unlocks, formats, or creates a path.
func Inspect(path string) (Evidence, error) { return platformInspect(path) }

// VerifyPersistent rejects storage that cannot be proved encrypted by Inspect.
// RAM storage is deliberately not accepted, even for a nonexistent target.
func VerifyPersistent(path string) error {
	_, err := Inspect(path)
	return err
}
