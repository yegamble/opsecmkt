package hostcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type savedReceipt struct {
	Receipt Receipt `json:"receipt"`
	BootID  string  `json:"boot_id"`
}

type ledger struct {
	Version       int                     `json:"version"`
	DeploymentDir string                  `json:"deployment_dir"`
	LastID        string                  `json:"last_id,omitempty"`
	Records       map[string]savedReceipt `json:"records"`
}

type ledgerStore struct {
	dir       string
	checkFile func(string) error
}

func (s *ledgerStore) load(deployment string, allowCreate bool) (ledger, error) {
	path := filepath.Join(s.dir, LedgerFileName)
	if _, err := os.Lstat(path); os.IsNotExist(err) && allowCreate {
		initial := ledger{Version: ProtocolVersion, DeploymentDir: deployment, Records: map[string]savedReceipt{}}
		return initial, s.save(initial)
	}
	if s.checkFile(path) != nil {
		return ledger{}, errors.New("host control receipt history is unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return ledger{}, errors.New("host control receipt history is unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return ledger{}, errors.New("host control receipt history is unavailable")
	}
	var result ledger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Version != ProtocolVersion || result.DeploymentDir != deployment || result.Records == nil || len(result.Records) > maxReceipts {
		return ledger{}, errors.New("host control receipt history is invalid")
	}
	for id, record := range result.Records {
		if !validRequestID(id) || record.Receipt.RequestID != id || !validState(record.Receipt.State) || !validBootID(record.BootID) || record.Receipt.AcceptedAt.IsZero() || !record.Receipt.RebootAt.Equal(record.Receipt.AcceptedAt.Add(rebootGrace)) {
			return ledger{}, errors.New("host control receipt history is invalid")
		}
	}
	if len(result.Records) != 0 {
		if _, ok := result.Records[result.LastID]; !ok {
			return ledger{}, errors.New("host control receipt history is invalid")
		}
	} else if result.LastID != "" {
		return ledger{}, errors.New("host control receipt history is invalid")
	}
	return result, nil
}

func (s *ledgerStore) save(value ledger) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 4<<20 {
		return errors.New("host control receipt history cannot be saved")
	}
	path := filepath.Join(s.dir, LedgerFileName)
	if _, err := os.Lstat(path); err == nil {
		if s.checkFile(path) != nil {
			return errors.New("host control receipt history cannot be saved")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("host control receipt history cannot be saved")
	}
	f, err := os.CreateTemp(s.dir, ".receipt-")
	if err != nil {
		return errors.New("host control receipt history cannot be saved")
	}
	name := f.Name()
	defer os.Remove(name)
	_, writeErr := f.Write(append(data, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || os.Rename(name, path) != nil {
		return errors.New("host control receipt history cannot be saved")
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return errors.New("host control receipt history cannot be saved")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errors.New("host control receipt history cannot be saved")
	}
	return nil
}

func cloneLedger(original ledger) ledger {
	result := original
	result.Records = make(map[string]savedReceipt, len(original.Records)+1)
	for id, receipt := range original.Records {
		result.Records[id] = receipt
	}
	return result
}

func validBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
		} else if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
