package market

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// AuditExportRetention follows the oldest included event, never the download or
// copy date. The metadata is outside the signed bytes.
func AuditExportRetention(data []byte) (time.Time, time.Time, error) {
	var first time.Time
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var event struct {
			Created string `json:"created"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return time.Time{}, time.Time{}, errors.New("invalid audit export retention timestamp")
		}
		t, err := time.Parse(time.RFC3339, event.Created)
		if err != nil || t.Before(time.Unix(0, 0)) {
			return time.Time{}, time.Time{}, errors.New("invalid audit export retention timestamp")
		}
		// Issued JSONL timestamps have second precision. The sidecar uses the
		// same UTC precision so host expiry does not depend on a local timezone
		// or acquire extra retention from fractional timestamp formatting.
		t = t.UTC().Truncate(time.Second)
		if first.IsZero() || t.Before(first) {
			first = t.UTC()
		}
	}
	if scanner.Err() != nil {
		return time.Time{}, time.Time{}, errors.New("invalid audit export retention data")
	}
	if first.IsZero() {
		first = time.Now().UTC().Truncate(time.Second)
	}
	return first, first.Add(180 * 24 * time.Hour), nil
}
