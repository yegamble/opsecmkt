package market

import (
	"html"

	"regexp"

	"strconv"

	"testing"
)

var (
	cosignerArea = regexp.MustCompile(`<pre class="code-block escrow-cosigner"[^>]*>([^<]+)</pre>`)
	practiceAddr = regexp.MustCompile(`<pre class="code-block escrow-practice-address"[^>]*>([^<]+)</pre>`)
	practicePath = regexp.MustCompile(`<pre class="code-block escrow-practice-path"[^>]*>([^<]+)</pre>`)
)

const untriedWallets = "The Electrum menu steps have not been checked, and Sparrow has not been tested"

// perOrderPractice reads the shape-(b) practice material of the viewer's page: the PSBT, the two cosigner keys, the
// practice wallet's first address and the owner's path.
func perOrderPractice(t *testing.T, e *testEnv, sess string) (psbt string, cosigners []string, addr, path string) {
	t.Helper()
	body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String()
	for _, m := range cosignerArea.FindAllStringSubmatch(body, -1) {
		cosigners = append(cosigners, html.UnescapeString(m[1]))
	}
	a, p := practiceAddr.FindStringSubmatch(body), practicePath.FindStringSubmatch(body)
	if len(cosigners) != 2 || a == nil || p == nil {
		t.Fatalf("per-order practice material missing: %s", body)
	}
	return practicePSBT(t, e, sess), cosigners, html.UnescapeString(a[1]), html.UnescapeString(p[1])
}

var readinessCount = regexp.MustCompile(`id="(escrow-readiness-[a-z]+)"><strong>(\d+)</strong>`)

func readinessCounts(page string) map[string]int {
	out := map[string]int{}
	for _, m := range readinessCount.FindAllStringSubmatch(page, -1) {
		out[m[1]], _ = strconv.Atoi(m[2])
	}
	return out
}
