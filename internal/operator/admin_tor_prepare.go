package operator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"opsecmkt/internal/onionidentity"
)

// PrepareAdminTor is the networkless init-container entry point. It copies only
// validated public authorization records into the dedicated Tor identity volume;
// existing onion identity bytes are never replaced or removed.
func PrepareAdminTor(args []string, out io.Writer) error {
	f := flag.NewFlagSet("admin-tor-prepare", flag.ContinueOnError)
	f.SetOutput(out)
	source := f.String("source", "", "read-only public client authorization snapshot")
	identitySource := f.String("identity-source", "", "read-only selected Tor identity; omitted only to preserve an already populated legacy volume")
	destination := f.String("destination", "", "dedicated Tor identity volume directory")
	uid := f.Int("uid", 0, "non-root Tor runtime user ID")
	gid := f.Int("gid", 0, "non-root Tor runtime group ID")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || !cleanAbsolutePath(*source) || !cleanAbsolutePath(*destination) || *source == "/" || *destination == "/" || *uid < 1 || *gid < 1 || uint64(*uid) > 1<<31-1 || uint64(*gid) > 1<<31-1 {
		return errors.New("admin-tor-prepare requires separate absolute source and destination directories and non-root numeric IDs")
	}
	rel, err := filepath.Rel(*source, *destination)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("administrator Tor authorization source and identity volume must be separate")
	}
	rel, err = filepath.Rel(*destination, *source)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("administrator Tor authorization source and identity volume must be separate")
	}
	if os.Geteuid() != 0 && (*uid != os.Geteuid() || *gid != os.Getegid()) {
		return errors.New("administrator Tor preparation requires permission to assign the configured runtime IDs")
	}
	// Capture and validate all records before touching the persistent volume.
	records, err := readAdminAuth(*source)
	if err != nil {
		return err
	}
	identity, err := selectedOnionIdentity(*identitySource, *destination)
	if err != nil {
		return err
	}
	if err := prepareAdminTorVolume(*destination, records, identity, *uid, *gid); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "administrator Tor authorization prepared; onion identity retained")
	return err
}

func prepareAdminTorVolume(destination string, records map[string][]byte, identity *onionidentity.Identity, uid, gid int) error {
	before, err := os.Lstat(destination)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("administrator Tor identity volume must be a real directory")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		return errors.New("administrator Tor identity directory changed")
	}
	entries, err := dir.ReadDir(-1)
	if err != nil || len(entries) > 1024 {
		return errors.New("administrator Tor identity volume cannot be inspected")
	}
	// Reject symlinks and unexpected directories before any ownership change.
	for _, entry := range entries {
		info, err := root.Lstat(entry.Name())
		if err != nil || (entry.Name() == "authorized_clients" && !info.IsDir()) || (entry.Name() != "authorized_clients" && !info.Mode().IsRegular()) {
			return errors.New("administrator Tor identity volume contains an unsafe entry")
		}
	}
	if err := prepareOnionFiles(root, dir, identity, uid, gid); err != nil {
		return err
	}
	if err := root.Mkdir("authorized_clients", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	clients, err := root.OpenRoot("authorized_clients")
	if err != nil {
		return err
	}
	defer clients.Close()
	clientDir, err := clients.Open(".")
	if err != nil {
		return err
	}
	defer clientDir.Close()
	clientEntries, err := clientDir.ReadDir(-1)
	if err != nil || len(clientEntries) > 1024 {
		return errors.New("administrator Tor authorization directory cannot be inspected")
	}
	for _, entry := range clientEntries {
		info, err := clients.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() || (!adminClientName.MatchString(entry.Name()) && !strings.HasPrefix(entry.Name(), ".admin-auth-")) {
			return errors.New("administrator Tor authorization volume contains an unsafe entry")
		}
	}
	for _, entry := range entries {
		if entry.Name() == "authorized_clients" {
			continue
		}
		file, err := root.Open(entry.Name())
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		prior, priorErr := root.Lstat(entry.Name())
		if statErr != nil || priorErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, prior) {
			file.Close()
			return errors.New("administrator Tor identity file changed")
		}
		err = setTorOwnership(file, uid, gid, 0600)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := setTorOwnership(dir, uid, gid, 0700); err != nil {
		return err
	}
	if err := setTorOwnership(clientDir, uid, gid, 0700); err != nil {
		return err
	}
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	// Install valid records first. Never remove/rename the directory or leave an
	// empty authorization window while updating an existing onion identity.
	for _, name := range names {
		if err := writeTorAuthorization(clients, name, records[name], uid, gid); err != nil {
			return err
		}
	}
	for _, entry := range clientEntries {
		if _, retained := records[entry.Name()]; !retained {
			if err := clients.Remove(entry.Name()); err != nil {
				return err
			}
		}
	}
	if err := clientDir.Sync(); err != nil {
		return err
	}
	return dir.Sync()
}

func setTorOwnership(file *os.File, uid, gid int, mode os.FileMode) error {
	if os.Geteuid() == 0 {
		if err := file.Chown(uid, gid); err != nil {
			return err
		}
	}
	return file.Chmod(mode)
}

func writeTorAuthorization(root *os.Root, name string, record []byte, uid, gid int) error {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := ".admin-auth-" + hex.EncodeToString(suffix[:])
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err = file.Write(record); err == nil {
		err = setTorOwnership(file, uid, gid, 0600)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temporary, name)
}
