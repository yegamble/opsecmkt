package market

import (
	"fmt"

	"os/exec"

	"regexp"

	"strings"
	"sync"

	"testing"

	"unicode/utf8"
)

type legLog struct {
	t   *testing.T
	mu  sync.Mutex
	buf strings.Builder
}

func (l *legLog) Logf(format string, args ...any) {
	l.t.Helper()
	s := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.buf.WriteString(s + "\n")
	l.mu.Unlock()
	l.t.Log(s)
}

func (l *legLog) Fatalf(format string, args ...any) {
	l.t.Helper()
	l.Logf(format, args...)
	l.t.FailNow()
}

type signerProc struct {
	cmd    *exec.Cmd
	socket string
	log    string
}

var tprvPattern = regexp.MustCompile(`tprv[1-9A-HJ-NP-Za-km-z]{100,}`)

// assertSecretNowhereInDB is assertNowhereInDB for a secret: on a hit it names the column and the label, never the
// needle. Text columns are searched only for a needle that is valid UTF-8 (PostgreSQL text cannot hold anything else);
// bytea columns always, with the needle's bytes.
func assertSecretNowhereInDB(t *testing.T, e *testEnv, needle []byte, label string) {
	t.Helper()
	rows, err := e.DB.Query(`SELECT table_name,column_name,data_type FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','character varying','bytea','jsonb','json')`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][3]string
	for rows.Next() {
		var c [3]string
		rows.Scan(&c[0], &c[1], &c[2])
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) < 20 {
		t.Fatalf("only %d columns found", len(cols))
	}
	for _, c := range cols {
		var arg any = string(needle)
		cond := `strpos("` + c[1] + `"::text, $1)>0`
		if c[2] == "bytea" {
			arg, cond = needle, `position($1::bytea in "`+c[1]+`")>0`
		} else if !utf8.Valid(needle) {
			continue
		}
		var n int
		if err := e.DB.QueryRow(`SELECT count(*) FROM "`+c[0]+`" WHERE `+cond, arg).Scan(&n); err != nil {
			t.Fatalf("%s.%s: query failed", c[0], c[1])
		}
		if n != 0 {
			t.Fatalf("%s.%s holds the %s", c[0], c[1], label)
		}
	}
}
