package ramsecrets

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"opsecmkt/internal/onionidentity"
)

type configMetadata struct {
	Version           int               `json:"version"`
	RuntimeUID        int               `json:"runtime_uid"`
	RuntimeGID        int               `json:"runtime_gid"`
	RetentionDir      string            `json:"retention_dir"`
	AdminAuthDir      string            `json:"admin_auth_dir"`
	AdminSocketDir    string            `json:"admin_socket_dir"`
	OnionDir          string            `json:"onion_dir"`
	OnionAddress      string            `json:"onion_address"`
	AdminOnionDir     string            `json:"admin_onion_dir"`
	AdminOnionAddress string            `json:"admin_onion_address"`
	SecretFiles       map[string]string `json:"secret_files"`
	PublicFiles       map[string]string `json:"public_files"`
	TLSFiles          map[string]string `json:"tls_files"`
}

func collect(dir string) ([]byte, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("secrets pack: source must be a private0700real installation directory")
	}
	source, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("secrets pack: invalid installation path")
	}
	bundle := Bundle{Version: 1, SourceDir: source}
	add := func(name string) error {
		data, err := readRegular(filepath.Join(dir, filepath.FromSlash(name)), !strings.HasPrefix(name, "admin-clients/"))
		if err != nil {
			return errors.New("secrets pack: installation files must be private regular files of at most64KiB")
		}
		bundle.Files = append(bundle.Files, File{Path: name, Data: data})
		return nil
	}
	if err := add("operator.json"); err != nil {
		return nil, err
	}
	var metadata configMetadata
	if err := json.Unmarshal(bundle.Files[0].Data, &metadata); err != nil {
		return nil, errors.New("secrets pack: operator metadata is invalid")
	}
	directories := []string{"secrets"}
	if metadata.AdminAuthDir != "" {
		if metadata.AdminAuthDir != "admin-clients" {
			return nil, errors.New("secrets pack: unsupported administrator client directory")
		}
		directories = append(directories, "admin-clients")
	}
	if metadata.OnionDir != "" {
		directories = append(directories, metadata.OnionDir)
	}
	if metadata.AdminOnionDir != "" {
		directories = append(directories, metadata.AdminOnionDir)
	}
	for _, identityDir := range []struct{ path, expected string }{{metadata.OnionDir, "onion-public"}, {metadata.AdminOnionDir, "onion-admin"}} {
		if identityDir.path != "" && identityDir.path != identityDir.expected {
			return nil, errors.New("secrets pack: unsupported onion identity directory")
		}
	}
	for _, name := range directories {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || name != "admin-clients" && info.Mode().Perm()&0077 != 0 || name == "admin-clients" && info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("secrets pack: managed key directories must be private real directories")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, errors.New("secrets pack: cannot enumerate managed key files")
		}
		for _, entry := range entries {
			if len(bundle.Files) >= maxFiles {
				return nil, errors.New("secrets pack: too many managed files")
			}
			if err := add(name + "/" + entry.Name()); err != nil {
				return nil, err
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "installation.json")); err == nil {
		if err := add("installation.json"); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("secrets pack: installation receipt cannot be inspected")
	}
	if _, err := validate(bundle); err != nil {
		return nil, err
	}
	sort.Slice(bundle.Files, func(i, j int) bool { return bundle.Files[i].Path < bundle.Files[j].Path })
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > maxBundle {
		return nil, errors.New("secrets pack: plaintext JSON bundle exceeds1MiB")
	}
	return data, nil
}

func validate(bundle Bundle) (configMetadata, error) {
	var metadata configMetadata
	if bundle.Version != 1 || len(bundle.Files) == 0 || len(bundle.Files) > maxFiles {
		return metadata, errors.New("secrets: invalid bundle version or file count")
	}
	if bundle.SourceDir != "" && (!filepath.IsAbs(bundle.SourceDir) || bundle.SourceDir != filepath.Clean(bundle.SourceDir) || strings.ContainsAny(bundle.SourceDir, "\x00\r\n")) {
		return metadata, errors.New("secrets: invalid bundle source metadata")
	}
	files := map[string][]byte{}
	total := 0
	for _, file := range bundle.Files {
		if !validPath(file.Path) || files[file.Path] != nil || len(file.Data) == 0 || len(file.Data) > maxFile {
			return metadata, errors.New("secrets: invalid, repeated or oversized bundle file")
		}
		if bytes.Contains(file.Data, []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----")) {
			return metadata, errors.New("secrets: private OpenPGP recovery keys must remain offline")
		}
		files[file.Path] = file.Data
		total += len(file.Data)
		if total > maxBundle {
			return metadata, errors.New("secrets: decoded bundle exceeds1MiB")
		}
	}
	config := files["operator.json"]
	d := json.NewDecoder(bytes.NewReader(config))
	if d.Decode(&metadata) != nil || d.Decode(new(any)) != io.EOF || metadata.Version != 1 || metadata.RuntimeUID < 1 || metadata.RuntimeGID < 1 || metadata.RuntimeUID > 1<<31-1 || metadata.RuntimeGID > 1<<31-1 {
		return metadata, errors.New("secrets: operator metadata requires version1and positive nonroot runtime IDs")
	}
	if !filepath.IsAbs(metadata.RetentionDir) || metadata.RetentionDir != filepath.Clean(metadata.RetentionDir) || metadata.RetentionDir == string(filepath.Separator) || strings.ContainsAny(metadata.RetentionDir, "\x00\r\n") {
		return metadata, errors.New("secrets: an explicit external persistent retention_dir is required")
	}
	required := map[string]bool{"operator.json": true, "installation.json": true}
	for _, role := range []struct{ dir, address, expected string }{{metadata.OnionDir, metadata.OnionAddress, "onion-public"}, {metadata.AdminOnionDir, metadata.AdminOnionAddress, "onion-admin"}} {
		if role.dir == "" && role.address == "" {
			continue
		}
		if role.dir != role.expected || role.address == "" {
			return metadata, errors.New("secrets: onion identity metadata is incomplete or unsupported")
		}
		secret, public, hostname := files[role.dir+"/hs_ed25519_secret_key"], files[role.dir+"/hs_ed25519_public_key"], files[role.dir+"/hostname"]
		if secret == nil || public == nil || hostname == nil {
			return metadata, errors.New("secrets: managed onion identities require their complete keypair and hostname")
		}
		identity, err := onionidentity.Parse(secret, public, string(hostname))
		if err != nil || identity.Address != role.address {
			return metadata, errors.New("secrets: onion keypair, hostname and configured address must match")
		}
		for _, name := range []string{"hs_ed25519_secret_key", "hs_ed25519_public_key", "hostname"} {
			required[role.dir+"/"+name] = true
		}
	}
	if metadata.OnionAddress != "" && metadata.OnionAddress == metadata.AdminOnionAddress {
		return metadata, errors.New("secrets: public and administrator onion identities must be distinct")
	}
	for _, paths := range []map[string]string{metadata.SecretFiles, metadata.PublicFiles, metadata.TLSFiles} {
		for _, path := range paths {
			if !validPath(path) || !strings.HasPrefix(path, "secrets/") || files[path] == nil {
				return metadata, errors.New("secrets: referenced key files must be present under secrets/")
			}
			required[path] = true
		}
	}
	if len(metadata.SecretFiles) == 0 {
		return metadata, errors.New("secrets: operator metadata has no referenced secrets")
	}
	if metadata.AdminAuthDir != "" && metadata.AdminAuthDir != "admin-clients" {
		return metadata, errors.New("secrets: unsupported administrator client directory")
	}
	authCount := 0
	for path, data := range files {
		if strings.HasPrefix(path, "admin-clients/") {
			if metadata.AdminAuthDir != "admin-clients" || !validAuth(data) {
				return metadata, errors.New("secrets: administrator authorization files must contain public descriptor:x25519 keys only")
			}
			authCount++
			continue
		}
		if !required[path] {
			return metadata, errors.New("secrets: bundle contains an unreferenced file")
		}
	}
	if metadata.AdminAuthDir != "" && authCount == 0 {
		return metadata, errors.New("secrets: administrator client directory has no authorization keys")
	}
	if receipt := files["installation.json"]; receipt != nil {
		var object map[string]json.RawMessage
		if len(receipt) > 4096 || json.Unmarshal(receipt, &object) != nil || object == nil {
			return metadata, errors.New("secrets: invalid installation receipt")
		}
	}
	return metadata, nil
}

func validPath(path string) bool {
	if path == "operator.json" || path == "installation.json" {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && (parts[0] == "onion-public" || parts[0] == "onion-admin") {
		return parts[1] == "hostname" || parts[1] == "hs_ed25519_secret_key" || parts[1] == "hs_ed25519_public_key"
	}
	if len(parts) != 2 || (parts[0] != "secrets" && parts[0] != "admin-clients") || len(parts[1]) == 0 || len(parts[1]) > 128 || parts[1][0] == '.' {
		return false
	}
	for _, ch := range parts[1] {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return parts[0] != "admin-clients" || strings.HasSuffix(parts[1], ".auth")
}
func validAuth(data []byte) bool {
	value := strings.TrimSuffix(string(data), "\n")
	encoded, ok := strings.CutPrefix(value, "descriptor:x25519:")
	if !ok || len(encoded) != 52 {
		return false
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encoded)
	return err == nil && len(raw) == 32
}

func readRegular(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (private && info.Mode().Perm()&0077 != 0) || (!private && info.Mode().Perm()&0022 != 0) {
		return nil, errors.New("file unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file unavailable")
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() || (private && current.Mode().Perm()&0077 != 0) || (!private && current.Mode().Perm()&0022 != 0) {
		return nil, errors.New("file changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(data) == 0 || len(data) > maxFile {
		return nil, errors.New("invalid file size")
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.reader.Read(p)
}

func unlock(ctx context.Context, dir string, in io.Reader, p platform) error {
	if !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || dir == string(filepath.Separator) {
		return errors.New("secrets unlock: destination must be a clean absolute instance path")
	}
	parent := filepath.Dir(dir)
	if err := p.verify(parent); err != nil {
		return errors.New("secrets unlock: parent must be a real root-owned0700directory on nosuid,nodev,noexec tmpfs")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return errors.New("secrets unlock: destination exists or cannot be inspected; nothing overwritten")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, in}, maxBundle+1))
	if err != nil || len(data) == 0 || len(data) > maxBundle {
		return errors.New("secrets unlock: plaintext stdin must contain at most1MiB of bundle JSON")
	}
	var bundle Bundle
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&bundle) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("secrets unlock: invalid plaintext bundle JSON")
	}
	metadata, err := validate(bundle)
	if err != nil {
		return err
	}
	if metadata.AdminSocketDir != "" && bundle.SourceDir != "" && metadata.AdminSocketDir == filepath.Join(bundle.SourceDir, "admin") {
		for i := range bundle.Files {
			if bundle.Files[i].Path == "operator.json" {
				var fields map[string]json.RawMessage
				if json.Unmarshal(bundle.Files[i].Data, &fields) != nil {
					return errors.New("secrets unlock: invalid operator metadata")
				}
				fields["admin_socket_dir"], _ = json.Marshal(filepath.Join(dir, "admin"))
				bundle.Files[i].Data, err = json.Marshal(fields)
				if err != nil {
					return errors.New("secrets unlock: cannot update runtime socket path")
				}
			}
		}
		if metadata, err = validate(bundle); err != nil {
			return err
		}
	}
	if metadata.RetentionDir == dir || strings.HasPrefix(metadata.RetentionDir, dir+string(filepath.Separator)) {
		return errors.New("secrets unlock: retention must be outside the RAM installation")
	}
	memory, err := p.isTmpfs(metadata.RetentionDir)
	if err != nil || memory {
		return errors.New("secrets unlock: retention must use separately verified persistent storage")
	}
	if ctx.Err() != nil {
		return errors.New("secrets unlock: operation cancelled")
	}
	stage, err := os.MkdirTemp(parent, ".opsecmkt-unlock-")
	if err != nil {
		return errors.New("secrets unlock: cannot stage RAM installation")
	}
	defer os.RemoveAll(stage)
	if err = os.Chmod(stage, 0700); err != nil {
		return errors.New("secrets unlock: cannot protect RAM staging directory")
	}
	directories := []string{"secrets"}
	if metadata.AdminAuthDir != "" {
		directories = append(directories, "admin-clients")
	}
	if metadata.OnionDir != "" {
		directories = append(directories, metadata.OnionDir)
	}
	if metadata.AdminOnionDir != "" {
		directories = append(directories, metadata.AdminOnionDir)
	}
	for _, name := range directories {
		if err := os.Mkdir(filepath.Join(stage, name), 0700); err != nil {
			return errors.New("secrets unlock: cannot create protected key directory")
		}
	}
	for _, file := range bundle.Files {
		path := filepath.Join(stage, filepath.FromSlash(file.Path))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("secrets unlock: cannot create RAM file exclusively")
		}
		_, writeErr := f.Write(file.Data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return errors.New("secrets unlock: cannot write RAM file")
		}
		if strings.HasPrefix(file.Path, "secrets/") {
			if err = p.chown(path, metadata.RuntimeUID, metadata.RuntimeGID); err != nil {
				return errors.New("secrets unlock: cannot assign runtime key ownership")
			}
		}
	}
	if err = p.chown(filepath.Join(stage, "secrets"), metadata.RuntimeUID, metadata.RuntimeGID); err != nil {
		return errors.New("secrets unlock: cannot assign runtime key-directory ownership")
	}
	if ctx.Err() != nil {
		return errors.New("secrets unlock: operation cancelled")
	}
	// Linux renameat2(RENAME_NOREPLACE) publishes only to an absent name.
	if err = p.publish(stage, dir); err != nil {
		return errors.New("secrets unlock: cannot publish RAM installation")
	}
	return nil
}
