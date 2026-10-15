package market

import (
	"net/http/httptest"

	"testing"
)

type bumpNonceResponse struct {
	code             int
	location, notice string
}

func bumpSubmissionCount(t *testing.T, e *testEnv) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump-sign'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func noticeValue(w *httptest.ResponseRecorder) string {
	if c := noticeSetCookie(w); c != nil {
		return c.Value
	}
	return ""
}
