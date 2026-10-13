package market

import (
	"strconv"
	"testing"
	"time"
)

func TestLimiterProtectedTOTPEntriesBoundCapacityAndExpire(t *testing.T) {
	limits := make(map[string]bucket)
	now := time.Now()
	until := now.Add(10 * time.Minute)
	for i := range 4096 {
		limits["totp-verify:"+strconv.Itoa(i)] = bucket{Count: 1, Limit: 10, Until: until}
	}
	if allowIn(limits, "setup:fresh", 10, now) || len(limits) != 4096 {
		t.Fatal("full protected table evicted a factor allowance or grew beyond its bound")
	}
	for i := range 4096 {
		if b, ok := limits["totp-verify:"+strconv.Itoa(i)]; !ok || b.Count != 1 || !b.Until.Equal(until) {
			t.Fatalf("active factor allowance %d was reset: %+v", i, b)
		}
	}
	if !allowIn(limits, "totp-verify:0", 10, now) || limits["totp-verify:0"].Count != 2 {
		t.Fatal("table saturation refused an existing factor's remaining allowance")
	}
	if !allowIn(limits, "setup:fresh", 10, until.Add(time.Nanosecond)) || len(limits) != 1 {
		t.Fatal("expired protected entries did not release capacity")
	}
	if !allowIn(limits, "totp-verify:0", 10, until.Add(time.Nanosecond)) || limits["totp-verify:0"].Count != 1 {
		t.Fatal("expired factor window did not start a fresh allowance")
	}
}
