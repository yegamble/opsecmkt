package hostbootstrap

import "encoding/json"

func marshal(v any) ([]byte, error) {
	b, e := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), e
}
