package market

import (
	"testing"
)

// Go's per-order origin check (checkEscrowOrigin) for shape (b): /<i>/0/0 with 2 <= i < 2^31-1, unhardened and
// canonical.
func TestEscrowPerOrderOrigin(t *testing.T) {
	k := mustParseKey(t, testBIP48Key)
	for suffix, ok := range map[string]bool{
		"/2/0/0": true, "/17/0/0": true, "/2147483646/0/0": true,
		"/0/0/0": false, "/1/0/0": false, "/2147483647/0/0": false, "/2147483648/0/0": false, "/2147483649/0/0": false,
		"/2/0/1": false, "/2/1/0": false, "/2h/0/0": false, "/02/0/0": false, "/2/0/0/0": false, "/2/0": false,
	} {
		if err := checkEscrowOrigin(k, k.PathText, suffix); (err == nil) != ok {
			t.Errorf("suffix %s: %v, want ok=%v", suffix, err, ok)
		}
	}
	if perOrderSuffix(5) != "/5/0/0" {
		t.Fatalf("perOrderSuffix(5) = %q", perOrderSuffix(5))
	}
}
