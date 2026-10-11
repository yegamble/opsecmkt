package market

import (
	"bytes"
	"strings"
	"testing"
)

// A-207: once the listing ID was validated, later multipart refusals return to that listing without storing a photo.
func TestUploadEarlyRefusalReturnsToParsedListing(t *testing.T) {
	e, scanner := uploadEnv(t)
	owner, session := e.user("early_backlink_vendor", "vendor")
	listing := e.product(owner, "physical")
	csrf, id := e.csrfPart(session), part{name: "id", body: []byte(listing)}
	for _, tc := range []struct {
		name   string
		status int
		parts  []part
		back   string
	}{
		{"oversized file", 413, e.photoParts(session, listing, bytes.Repeat([]byte("x"), uploadCap+1)), "/listing-edit?id=" + listing + "#photos"},
		{"no photo", 400, []part{csrf, id}, "/listing-edit?id=" + listing + "#photos"},
		{"invalid description", 400, []part{csrf, id, {name: "description", body: []byte("x\x00y")}}, "/listing-edit?id=" + listing + "#photos"},
		{"invalid id", 400, []part{csrf, {name: "id", body: []byte("x\x00y")}}, "/vendor-dashboard"},
		{"photo before id", 400, []part{csrf, {name: "photo", body: []byte("x")}, id}, "/vendor-dashboard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := e.upload("/listings/photo", session, tc.parts...)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `href="`+tc.back+`"`) {
				t.Fatalf("status=%d, want %d and backlink %q: %s", response.Code, tc.status, tc.back, response.Body.String())
			}
			label := "Back to the listing"
			if tc.back == "/vendor-dashboard" {
				label = "Back to the vendor desk"
			}
			if !strings.Contains(response.Body.String(), label) {
				t.Fatalf("missing backlink label %q", label)
			}
			if e.imageCount() != 0 || scansOf(scanner) != 0 {
				t.Fatal("refused form scanned or stored an image")
			}
		})
	}
}
