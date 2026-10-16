package onionidentity

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Read imports a private native Tor service directory. Missing public/hostname
// files are derived; existing ones are strictly checked rather than trusted.
func Read(dir string) (Identity, error) {
	root, err := openPrivateRoot(dir)
	if err != nil {
		return Identity{}, err
	}
	defer root.Close()
	secret, err := readRootFile(root, SecretFilename, 96, true)
	if err != nil {
		return Identity{}, err
	}
	public, err := optionalRootFile(root, PublicFilename, 64)
	if err != nil {
		return Identity{}, err
	}
	hostname, err := optionalRootFile(root, HostnameFilename, 63)
	if err != nil {
		return Identity{}, err
	}
	if hostname != nil && len(hostname) == 0 {
		return Identity{}, errors.New("onion identity: empty hostname file")
	}
	return Parse(secret, public, string(hostname))
}

// Import additionally requires the operator's full expected address.
func Import(dir, expectedAddress string) (Identity, error) {
	if err := ValidateAddress(expectedAddress); err != nil {
		return Identity{}, err
	}
	id, err := Read(dir)
	if err != nil {
		return Identity{}, err
	}
	if id.Address != expectedAddress {
		return Identity{}, errors.New("onion identity: expected address does not match imported identity")
	}
	return id, nil
}

func openPrivateRoot(dir string) (*os.Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil || checkDirectoryPath(abs) != nil {
		return nil, errors.New("onion identity: directory must be real and have no symlink components")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("onion identity: directory must be private with mode 0700")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, errors.New("onion identity: cannot open private directory")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("onion identity: directory changed while opening")
	}
	return root, nil
}

func checkDirectoryPath(path string) error {
	for {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe directory")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func readRootFile(root *os.Root, name string, limit int64, private bool) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("onion identity: input must be a bounded regular file with safe permissions and no symlink")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, errors.New("onion identity: cannot read input file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > limit || opened.Mode().Perm()&0022 != 0 || private && opened.Mode().Perm()&0077 != 0 {
		return nil, errors.New("onion identity: input changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("onion identity: cannot read bounded input file")
	}
	return b, nil
}

func optionalRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	if _, err := root.Lstat(name); os.IsNotExist(err) {
		return nil, nil
	}
	return readRootFile(root, name, limit, false)
}

func readInputFile(path string, limit int64, private bool) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil || checkDirectoryPath(filepath.Dir(abs)) != nil {
		return nil, errors.New("onion identity: input path must have no symlink components")
	}
	root, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, errors.New("onion identity: cannot open input directory")
	}
	defer root.Close()
	return readRootFile(root, filepath.Base(abs), limit, private)
}

func preflightNewDirectory(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil || checkDirectoryPath(filepath.Dir(abs)) != nil {
		return "", errors.New("onion identity: output parent must exist and have no symlink components")
	}
	if _, err := os.Lstat(abs); !os.IsNotExist(err) {
		return "", errors.New("onion identity: output directory must not already exist")
	}
	return abs, nil
}

// WriteNew writes a fresh private Tor directory. Existing destinations, files
// and symlinks are never overwritten. A failed write removes only files this
// call created, without following a replaced directory path.
func (id Identity) WriteNew(dir string) error {
	if err := ValidateAddress(id.Address); err != nil {
		return err
	}
	if _, err := Parse(id.SecretFile(), id.PublicFile(), id.Address); err != nil {
		return err
	}
	abs, err := preflightNewDirectory(dir)
	if err != nil {
		return err
	}
	if err := os.Mkdir(abs, 0700); err != nil {
		return errors.New("onion identity: output directory must not already exist")
	}
	root, err := openPrivateRoot(abs)
	if err != nil {
		os.Remove(abs)
		return err
	}
	defer root.Close()
	created := []string{}
	complete := false
	defer func() {
		if !complete {
			for _, name := range created {
				root.Remove(name)
			}
			os.Remove(abs)
		}
	}()
	for _, file := range []struct {
		name string
		data []byte
	}{
		{SecretFilename, id.SecretFile()}, {PublicFilename, id.PublicFile()}, {HostnameFilename, id.HostnameFile()},
	} {
		f, err := root.OpenFile(file.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("onion identity: cannot create private output file")
		}
		created = append(created, file.name)
		_, writeErr := f.Write(file.data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return errors.New("onion identity: cannot finish private output file")
		}
	}
	complete = true
	return nil
}
