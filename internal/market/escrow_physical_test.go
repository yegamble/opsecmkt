package market

import (
	"testing"
)

func physString(t *testing.T, e *testEnv, q string, args ...any) string {
	t.Helper()
	var s string
	if err := e.DB.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
