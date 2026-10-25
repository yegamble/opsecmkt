package hostcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Protocol objects contain no nested authority. Reject repeated keys as well
// as unknown fields, trailing values and incompatible field types.
func decodeObject(data []byte, result any) error {
	invalid := errors.New("invalid host control object")
	scan := json.NewDecoder(bytes.NewReader(data))
	first, err := scan.Token()
	if err != nil || first != json.Delim('{') {
		return invalid
	}
	seen := map[string]bool{}
	for scan.More() {
		key, err := scan.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return invalid
		}
		seen[name] = true
		var value json.RawMessage
		if scan.Decode(&value) != nil {
			return invalid
		}
	}
	if last, err := scan.Token(); err != nil || last != json.Delim('}') || scan.Decode(new(any)) != io.EOF {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil {
		return invalid
	}
	return nil
}
