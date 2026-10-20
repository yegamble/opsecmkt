package market

import (
	"testing"
)

func agStrings(t *testing.T, e *testEnv, q string, args ...any) []string {
	t.Helper()
	rows, err := e.DB.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
