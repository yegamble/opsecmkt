package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
	"path/filepath"
	"time"
)

// exportAudit writes a private, detached-signature pair from the existing database. No application starts.
// Exclusive creation refuses either existing output; partial outputs this invocation created are removed.
func exportAudit(ctx context.Context, from, upto int64, dir string, given func(string) bool, args []string, out io.Writer) error {
	if !given("export-audit") {
		return errors.New("-export-audit-from and -export-audit-dir require -export-audit N")
	}
	for _, other := range []string{"preview", "healthcheck", "status", "reset-admin-password", "reset-admin-factors", "set-role", "bootstrap-admin", "site-name"} {
		if given(other) {
			return errors.New("cannot be combined with -" + other)
		}
	}
	if len(args) > 0 || from < 0 || upto < 1 || from > upto {
		return errors.New("use -export-audit N with positive N and an optional nonnegative -export-audit-from no greater than N")
	}
	if dir == "" {
		return errors.New("-export-audit-dir must name an existing output directory")
	}
	recipient, err := market.LoadAuditExportRecipient(os.Getenv("AUDIT_EXPORT_RECIPIENT"))
	if err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	return withHostDB(ctx, dsn, func(ctx context.Context, db *sql.DB) error {
		data, sig, err := market.ExportAudit(ctx, db, from, upto)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			_, err = fmt.Fprintln(out, "No retained audit events in the requested range; no files created.")
			return err
		}
		created, expires, err := market.AuditExportRetention(data)
		if err != nil {
			return err
		}
		// Signatures are over plaintext; encrypt both in memory before creating
		// either output so a configured recipient never leaves plaintext files.
		if data, err = recipient.Encrypt(data); err != nil {
			return err
		}
		if sig, err = recipient.Encrypt(sig); err != nil {
			return err
		}
		name := filepath.Join(dir, market.AuditExportName(from, upto))
		jsonPath, sigPath := name+".jsonl"+recipient.Suffix(), name+".sig"+recipient.Suffix()
		jsonFile, err := os.OpenFile(jsonPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot create export file; use an existing writable directory and a new range")
		}
		complete := false
		defer func() {
			jsonFile.Close()
			if !complete {
				os.Remove(jsonPath)
			}
		}()
		metadataPath := jsonPath + ".retention.json"
		metadataFile, err := os.OpenFile(metadataPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot create audit retention metadata; use a new output range")
		}
		defer func() {
			metadataFile.Close()
			if !complete {
				os.Remove(metadataPath)
			}
		}()
		fileHash := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
		dataHash, sigHash := fileHash(data), fileHash(sig)
		metadata := struct {
			Version int                 `json:"version"`
			Kind    string              `json:"kind"`
			SHA256  string              `json:"sha256"`
			Created string              `json:"created_at"`
			Expires string              `json:"expires_at"`
			Files   []map[string]string `json:"files"`
		}{1, "audit_export", fileHash([]byte(dataHash + "\n" + sigHash)), created.Format(time.RFC3339), expires.Format(time.RFC3339),
			[]map[string]string{{"name": filepath.Base(jsonPath), "sha256": dataHash}, {"name": filepath.Base(sigPath), "sha256": sigHash}}}
		if err = json.NewEncoder(metadataFile).Encode(metadata); err == nil {
			err = metadataFile.Sync()
		}
		if err == nil {
			err = metadataFile.Close()
		}
		if err != nil {
			return errors.New("unable to write audit retention metadata")
		}
		sigFile, err := os.OpenFile(sigPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot create signature file; use an existing writable directory and a new range")
		}
		defer func() {
			sigFile.Close()
			if !complete {
				os.Remove(sigPath)
			}
		}()
		if _, err = jsonFile.Write(data); err == nil {
			err = jsonFile.Sync()
		}
		if err == nil {
			_, err = sigFile.Write(sig)
		}
		if err == nil {
			err = sigFile.Sync()
		}
		if err == nil {
			err = jsonFile.Close()
		}
		if err == nil {
			err = sigFile.Close()
		}
		if err != nil {
			return errors.New("unable to write complete audit export and signature")
		}
		complete = true
		instruction := "Keep both private; verify with verify-audit and your pinned public key."
		if recipient != nil {
			instruction = "Decrypt both offline, then verify with verify-audit and your pinned signing public key."
		}
		_, err = fmt.Fprintf(out, "Saved %s and %s with retention metadata %s (expires %s). %s\n", jsonPath, sigPath, metadataPath, expires.Format(time.RFC3339), instruction)
		return err
	})
}
