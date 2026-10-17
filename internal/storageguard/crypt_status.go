package storageguard

import (
	"errors"
	"strconv"
	"strings"
)

// cryptsetup status reports target properties, not the encryption key. Keep its
// raw output private and never use dmsetup table or --showkeys as a substitute.
func validateCryptStatus(data []byte) error {
	invalid := errors.New("cryptsetup did not confirm an active LUKS2 target with a non-null cipher and at least 256 key bits")
	if len(data) == 0 || len(data) > 16<<10 {
		return invalid
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.HasSuffix(lines[0], " is active.") && !strings.HasSuffix(lines[0], " is active and is in use.") {
		return invalid
	}
	values := map[string]string{}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key != "type" && key != "cipher" && key != "keysize" {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return invalid
		}
		values[key] = strings.TrimSpace(value)
	}
	cipher := values["cipher"]
	size := strings.Fields(values["keysize"])
	if values["type"] != "LUKS2" || len(strings.Fields(cipher)) != 1 || strings.Contains(strings.ToLower(cipher), "null") || len(size) != 2 || size[1] != "bits" {
		return invalid
	}
	bits, err := strconv.ParseUint(size[0], 10, 16)
	if err != nil || bits < 256 {
		return invalid
	}
	return nil
}
