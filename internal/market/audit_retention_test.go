package market

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAuditExportRetentionUsesEarliestEventAcrossIDOrder(t *testing.T) {
	data := []byte("{\"id\":1,\"created\":\"2026-09-14T12:00:00Z\"}\n" +
		"{\"id\":2,\"created\":\"2026-01-01T03:04:05.999+02:00\"}\n" +
		"{\"id\":3,\"created\":\"2026-06-01T00:00:00Z\"}\n")
	before := bytes.Clone(data)
	created, expires, err := AuditExportRetention(data)
	want := time.Date(2026, time.January, 1, 1, 4, 5, 0, time.UTC)
	if err != nil || !created.Equal(want) || !expires.Equal(want.Add(180*24*time.Hour)) {
		t.Fatalf("created=%s expires=%s err=%v", created, expires, err)
	}
	if created.Location() != time.UTC || !bytes.Equal(before, data) {
		t.Fatal("retention metadata changed issued bytes or kept a local timezone")
	}
}

func TestAuditExportRetentionEmptyRangeHasBoundedClock(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)
	created, expires, err := AuditExportRetention(nil)
	after := time.Now().UTC().Truncate(time.Second)
	if err != nil || created.Before(before) || created.After(after) || expires.Sub(created) != 180*24*time.Hour {
		t.Fatalf("empty range created=%s expires=%s err=%v", created, expires, err)
	}
}

func TestAuditExportRetentionRefusesMalformedTimestampsWithoutPrivateContent(t *testing.T) {
	for _, data := range []string{
		"private marker is not JSON\n", "{\"action\":\"private marker\"}\n",
		"{\"created\":\"private marker\"}\n", "{\"created\":null,\"action\":\"private marker\"}\n",
		"{\"created\":\"1969-12-31T23:59:59Z\",\"action\":\"private marker\"}\n",
		"{\"created\":\"2026-01-01T00:00:00Z\"}\n\n",
		"{\"created\":\"2026-01-01T00:00:00Z\",\"action\":\"" + strings.Repeat("x", 1<<20) + "\"}\n",
	} {
		created, expires, err := AuditExportRetention([]byte(data))
		if err == nil || !created.IsZero() || !expires.IsZero() || strings.Contains(err.Error(), "private marker") {
			t.Fatalf("invalid export retention: created=%s expires=%s err=%v", created, expires, err)
		}
	}
}
