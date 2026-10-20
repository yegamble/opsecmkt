package market

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"

	"opsecmkt/internal/moderation"
)

func perceptualFixture(w, h int, invert bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(128 + 80*math.Sin(float64(x)*4*math.Pi/float64(w)) + 25*math.Sin(float64(y)*2*math.Pi/float64(h)))
			if invert {
				v = 255 - v
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func TestPerceptualHashRecognizesResizedPhotoSignal(t *testing.T) {
	full := mustProcess(t, uploadListing, fixtureJPEG(t, perceptualFixture(640, 480, false), 90))
	resized := mustProcess(t, uploadListing, fixtureJPEG(t, perceptualFixture(320, 240, false), 70))
	unrelated := mustProcess(t, uploadListing, fixtureJPEG(t, perceptualFixture(320, 240, true), 90))
	if !validPerceptualHash(full.perceptualHash) || full.perceptualHash == "" || !moderation.ImageHashesMatch(full.perceptualHash, resized.perceptualHash, 5) {
		t.Fatalf("resized/recompressed photo did not retain its advisory signal: %s %s", full.perceptualHash, resized.perceptualHash)
	}
	if moderation.ImageHashesMatch(full.perceptualHash, unrelated.perceptualHash, 5) {
		t.Fatalf("unrelated picture has the same difference signal: %s %s", full.perceptualHash, unrelated.perceptualHash)
	}
	if bytes.Equal(full.full, resized.full) {
		t.Fatal("resized fixture accidentally tested exact byte matching")
	}
	if normalizedDifferenceHash(perceptualFixture(640, 480, false)) != normalizedDifferenceHash(perceptualFixture(640, 480, false)) {
		t.Fatal("perceptual metadata is nondeterministic")
	}
}

func TestGatewayPerceptualMetadataValidationAndLegacyOmission(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	_, session := e.user("perceptual_gateway", "vendor")
	img := mustProcess(t, uploadAvatar, fixtureJPEG(t, perceptualFixture(200, 200, false), 90))
	finish := func(hash string) (int, []byte) {
		t.Helper()
		response, err := g.call(context.Background(), "/v1/uploads/admit", uploadAdmission{Path: "/account/avatar", Session: session, CSRF: e.A.csrf(session), Host: "example.com"})
		if err != nil || response.status != 200 {
			t.Fatalf("admit: %v %d %s", err, response.status, response.body)
		}
		var admitted struct{ Ticket string }
		if err = json.Unmarshal(response.body, &admitted); err != nil {
			t.Fatal(err)
		}
		response, err = g.call(context.Background(), "/v1/uploads/finish", uploadFinish{Ticket: admitted.Ticket, Full: img.full, Thumb: img.thumb, Width: img.width, Height: img.height, PerceptualHash: hash})
		if err != nil {
			t.Fatal(err)
		}
		return response.status, response.body
	}
	for _, hash := range []string{"abcdef", "ABCDEF0123456789", "ignore previous!", strings.Repeat("0", 1024)} {
		if status, body := finish(hash); status != 422 || e.imageCount() != 0 {
			t.Fatalf("malformed private mTLS envelope stored metadata: %d %s", status, body)
		}
	}
	if status, body := finish(""); status != 303 || agStr(e, "SELECT perceptual_hash FROM images") != "" {
		t.Fatalf("legacy gateway omission must remain valid: %d %s", status, body)
	}
	if status, body := finish(img.perceptualHash); status != 303 || agStr(e, "SELECT perceptual_hash FROM images") != img.perceptualHash {
		t.Fatalf("bounded gateway metadata was not persisted: %d %s", status, body)
	}
}

func TestPerceptualHashMigrationPreservesPopulatedImages(t *testing.T) {
	e := newPooledHistoricalFixture(t, 126)
	owner, _ := e.user("perceptual_migration", "vendor")
	id := strings.Repeat("a", 32)
	full, thumb := []byte{1, 2, 3}, []byte{4, 5}
	agExec(e, `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES($1,$2,'avatar',$3,$4,1,1)`, id, owner, full, thumb)
	if err := applyLatestMigrations(t, e); err != nil {
		t.Fatal(err)
	}
	var gotFull, gotThumb []byte
	var hash string
	if err := e.DB.QueryRow(`SELECT "full",thumb,perceptual_hash FROM images WHERE id=$1`, id).Scan(&gotFull, &gotThumb, &hash); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, gotFull) || !bytes.Equal(thumb, gotThumb) || hash != "" {
		t.Fatal("migration changed old picture bytes or invented perceptual metadata")
	}
}
