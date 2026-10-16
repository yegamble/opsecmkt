package keymaterial

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

type outputFile struct {
	path string
	body []byte
	mode os.FileMode
}

func readFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("keys: input must be a readable regular file, without a symlink")
	}
	if private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("keys: private input file requires mode 0600 or stricter")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("keys: cannot read key input")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || (private && actual.Mode().Perm()&0077 != 0) {
		return nil, errors.New("keys: input file changed or is not private")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) == 0 || len(data) > 65536 {
		return nil, errors.New("keys: key input must hold between 1 byte and 64 KiB")
	}
	return data, nil
}

// writeFiles stages protected files in their destination directories and publishes
// with hard links, whose exclusive creation refuses files and symlinks alike.
// On failure only files this invocation created are removed.
func writeFiles(files []outputFile) error {
	seen := map[string]bool{}
	for i := range files {
		path, err := filepath.Abs(files[i].path)
		if err != nil || files[i].path == "" {
			return errors.New("keys: invalid output path")
		}
		if seen[path] {
			return errors.New("keys: output paths must be distinct")
		}
		seen[path] = true
		files[i].path = path
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			return errors.New("keys: output already exists or cannot be inspected; nothing replaced")
		}
	}
	var staged, published []string
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}()
	rollback := func() {
		for _, path := range published {
			_ = os.Remove(path)
		}
	}
	for _, file := range files {
		tmp, err := os.CreateTemp(filepath.Dir(file.path), ".opsecmkt-key-*")
		if err != nil {
			return errors.New("keys: cannot stage key output")
		}
		staged = append(staged, tmp.Name())
		err = tmp.Chmod(file.mode)
		if err == nil {
			_, err = tmp.Write(file.body)
		}
		if err == nil {
			err = tmp.Sync()
		}
		cerr := tmp.Close()
		if err != nil || cerr != nil {
			return errors.New("keys: cannot write protected key output")
		}
	}
	for i, file := range files {
		if err := os.Link(staged[i], file.path); err != nil {
			rollback()
			return errors.New("keys: cannot create output exclusively; nothing replaced")
		}
		published = append(published, file.path)
	}
	return nil
}
