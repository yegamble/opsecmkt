package operator

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"opsecmkt/internal/onionidentity"
)

var onionFileNames = []string{"hs_ed25519_public_key", "hostname", "hs_ed25519_secret_key"}

func selectedOnionIdentity(source, destination string) (*onionidentity.Identity, error) {
	if source == "" {
		return nil, nil
	}
	if !cleanAbsolutePath(source) || source == "/" {
		return nil, errors.New("onion identity source must be a private absolute directory")
	}
	for _, pair := range [][2]string{{source, destination}, {destination, source}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return nil, errors.New("onion identity source and destination must be separate")
		}
	}
	identity, err := onionidentity.Read(source)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// PrepareTorIdentity initializes a separate public onion identity volume without
// networking or application secrets. Existing keys are compared, never replaced.
func PrepareTorIdentity(args []string, out io.Writer) error {
	f := flag.NewFlagSet("tor-identity-prepare", flag.ContinueOnError)
	f.SetOutput(out)
	source := f.String("identity-source", "", "selected private Tor identity; omitted only for a populated legacy volume")
	destination := f.String("destination", "", "dedicated Tor identity volume directory")
	subdir := f.String("subdir", "", "optional marketplace service directory inside the volume")
	uid := f.Int("uid", 0, "non-root Tor runtime user ID")
	gid := f.Int("gid", 0, "non-root Tor runtime group ID")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || !cleanAbsolutePath(*destination) || *destination == "/" || (*subdir != "" && *subdir != "marketplace") || *uid < 1 || *gid < 1 || uint64(*uid) > 1<<31-1 || uint64(*gid) > 1<<31-1 {
		return errors.New("tor-identity-prepare requires an absolute dedicated destination, non-root IDs and optional marketplace subdirectory")
	}
	if os.Geteuid() != 0 && (*uid != os.Geteuid() || *gid != os.Getegid()) {
		return errors.New("onion identity preparation requires permission to assign configured runtime IDs")
	}
	identity, err := selectedOnionIdentity(*source, *destination)
	if err != nil {
		return err
	}
	info, err := os.Lstat(*destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("onion identity volume must be a real directory")
	}
	root, err := os.OpenRoot(*destination)
	if err != nil {
		return err
	}
	defer root.Close()
	volume, err := root.Open(".")
	if err != nil {
		return err
	}
	defer volume.Close()
	actual, err := volume.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return errors.New("onion identity volume changed")
	}
	serviceRoot := root
	if *subdir != "" {
		before, err := root.Lstat(*subdir)
		if os.IsNotExist(err) && identity != nil {
			err = root.Mkdir(*subdir, 0700)
		} else if err == nil && (!before.IsDir() || before.Mode()&os.ModeSymlink != 0) {
			err = errors.New("onion service directory must be a real directory")
		}
		if err != nil {
			return errors.New("cannot prepare onion service directory; preserve and review any existing identity volume")
		}
		serviceRoot, err = root.OpenRoot(*subdir)
		if err != nil {
			return err
		}
		defer serviceRoot.Close()
	}
	serviceDir, err := serviceRoot.Open(".")
	if err != nil {
		return err
	}
	defer serviceDir.Close()
	if err := prepareOnionFiles(serviceRoot, serviceDir, identity, *uid, *gid); err != nil {
		return err
	}
	if err := setTorOwnership(volume, *uid, *gid, 0700); err != nil {
		return err
	}
	if err := volume.Sync(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Tor onion identity prepared; existing identity keys retained")
	return err
}

func readVolumeOnionFile(root *os.Root, name string, max int64) ([]byte, bool, error) {
	before, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, false, errors.New("onion identity volume contains an unsafe key file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, false, errors.New("onion identity volume key changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, false, errors.New("onion identity volume key is unreadable or invalid")
	}
	return data, true, nil
}

func prepareOnionFiles(root *os.Root, dir *os.File, selected *onionidentity.Identity, uid, gid int) error {
	files := make(map[string][]byte, len(onionFileNames))
	present := 0
	for _, name := range onionFileNames {
		data, exists, err := readVolumeOnionFile(root, name, 96)
		if err != nil {
			return err
		}
		if exists {
			present++
			files[name] = data
		}
	}
	if present != 0 && present != len(onionFileNames) {
		return errors.New("onion identity volume is partial; preserve it and explicitly review/export/adopt its identity before retrying")
	}
	if present != 0 {
		existing, err := onionidentity.Parse(files["hs_ed25519_secret_key"], files["hs_ed25519_public_key"], string(files["hostname"]))
		if err != nil || (string(files["hostname"]) != existing.Address && string(files["hostname"]) != existing.Address+"\n") {
			return errors.New("onion identity volume is invalid; preserve it for explicit review")
		}
		if selected != nil && (existing.Address != selected.Address || !bytes.Equal(existing.SecretFile(), selected.SecretFile()) || !bytes.Equal(existing.PublicFile(), selected.PublicFile())) {
			return errors.New("selected onion identity differs from the populated volume; existing keys were retained; explicit reviewed export/adoption is required")
		}
	} else {
		if selected == nil {
			return errors.New("legacy onion volume has no identity; explicitly generate/import and review an identity before startup")
		}
		inspect, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := inspect.ReadDir(-1)
		inspect.Close()
		if readErr != nil || len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "authorized_clients") {
			return errors.New("unmanaged populated onion volume requires explicit reviewed export/adoption; no keys were replaced")
		}
		files["hs_ed25519_public_key"] = selected.PublicFile()
		files["hostname"] = selected.HostnameFile()
		files["hs_ed25519_secret_key"] = selected.SecretFile()
		for _, name := range onionFileNames {
			file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return errors.New("onion identity volume changed; no existing key was replaced")
			}
			if _, err = file.Write(files[name]); err == nil {
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
		}
	}
	for _, name := range onionFileNames {
		file, err := root.Open(name)
		if err != nil {
			return err
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
	return dir.Sync()
}
