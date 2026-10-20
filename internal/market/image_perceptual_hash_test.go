package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/bits"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/image/draw"

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
	// Inverting this sinusoid gives the same horizontal gradients as mirroring
	// it, so the mirror-aware comparison treats it as a mirrored copy.
	inverted := mustProcess(t, uploadListing, fixtureJPEG(t, perceptualFixture(320, 240, true), 90))
	unrelated := mustProcess(t, uploadListing, fixtureJPEG(t, texturedFixture(7, 320, 240), 90))
	if !validPerceptualHash(full.perceptualHash) || full.perceptualHash == "" || !moderation.ImageHashesMatch(full.perceptualHash, resized.perceptualHash, 5) {
		t.Fatalf("resized/recompressed photo did not retain its advisory signal: %s %s", full.perceptualHash, resized.perceptualHash)
	}
	if hashDistance(t, full.perceptualHash, inverted.perceptualHash) <= 5 || !moderation.ImageHashesMatch(full.perceptualHash, inverted.perceptualHash, 5) {
		t.Fatalf("mirror-equivalent picture: %s %s", full.perceptualHash, inverted.perceptualHash)
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

func TestPerceptualHashSkipsWeakPixelSignals(t *testing.T) {
	cases := map[string]func(int, int) uint8{
		"uniform dark":          func(_, _ int) uint8 { return 0 },
		"uniform bright":        func(_, _ int) uint8 { return 255 },
		"horizontal ramp":       func(x, _ int) uint8 { return uint8(x * 31) },
		"vertical ramp":         func(_, y int) uint8 { return uint8(y * 35) },
		"decreasing ramp":       func(x, _ int) uint8 { return uint8(248 - x*31) },
		"low contrast balanced": func(x, _ int) uint8 { return uint8(100 + x%2) },
	}
	for name, pixel := range cases {
		t.Run(name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, 9, 8))
			for y := range 8 {
				for x := range 9 {
					value := pixel(x, y)
					img.SetRGBA(x, y, color.RGBA{value, value, value, 255})
				}
			}
			if hash := normalizedDifferenceHash(img); hash != "" {
				t.Fatalf("weak pixels produced a misleading perceptual signal: %s", hash)
			}
		})
	}
}

func TestPerceptualWeakImagesRetainExactDuplicateSignal(t *testing.T) {
	flat := func(value uint8) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 80, 80))
		for y := range 80 {
			for x := range 80 {
				img.SetRGBA(x, y, color.RGBA{value, value, value, 255})
			}
		}
		return img
	}
	dark := mustProcess(t, uploadListing, fixturePNG(t, flat(0)))
	same := mustProcess(t, uploadListing, fixturePNG(t, flat(0)))
	bright := mustProcess(t, uploadListing, fixturePNG(t, flat(255)))
	for _, img := range []encodedImage{dark, same, bright} {
		if img.perceptualHash != "" || len(img.full) == 0 || img.width != 80 || img.height != 80 {
			t.Fatal("weak-hash suppression must preserve a valid normalized upload")
		}
	}
	sha := func(data []byte) string {
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:])
	}
	if !moderation.ImageHashesMatch(sha(dark.full), sha(same.full), 5) {
		t.Fatal("weak pixels lost the exact normalized SHA256 duplicate signal")
	}
	if moderation.ImageHashesMatch(sha(dark.full), sha(bright.full), 5) {
		t.Fatal("different weak images were reported as exact duplicates")
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

// texturedFixture is seeded coarse picture structure plus fine pixel-level
// stripes and noise. The smooth sinusoid above hides point-sampling aliasing;
// this texture exposes it, because a sampled pixel can land on a stripe or a
// noise peak instead of representing its whole cell.
func texturedFixture(seed int64, w, h int) *image.RGBA {
	r := rand.New(rand.NewSource(seed))
	const gx, gy = 12, 9
	var grid [gy + 1][gx + 1]float64
	for y := range grid {
		for x := range grid[y] {
			grid[y][x] = 48 + 160*r.Float64()
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			fx, fy := float64(x)*gx/float64(w), float64(y)*gy/float64(h)
			ix, iy := int(fx), int(fy)
			tx, ty := fx-float64(ix), fy-float64(iy)
			v := grid[iy][ix]*(1-tx)*(1-ty) + grid[iy][ix+1]*tx*(1-ty) + grid[iy+1][ix]*(1-tx)*ty + grid[iy+1][ix+1]*tx*ty
			if (x/2)%2 == 0 {
				v += 35
			} else {
				v -= 35
			}
			v += 14 * r.NormFloat64()
			c := uint8(max(0, min(255, v)))
			img.SetRGBA(x, y, color.RGBA{c, c, c, 255})
		}
	}
	return img
}

func scaledFixture(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Rect, src, src.Bounds(), draw.Src, nil)
	return dst
}

func hashDistance(t *testing.T, a, b string) int {
	t.Helper()
	x, errX := strconv.ParseUint(a, 16, 64)
	y, errY := strconv.ParseUint(b, 16, 64)
	if errX != nil || errY != nil {
		t.Fatalf("invalid perceptual hashes %q %q", a, b)
	}
	return bits.OnesCount64(x ^ y)
}

func TestPerceptualHashSurvivesRecompressResizeCrop(t *testing.T) {
	base := texturedFixture(72, 1200, 900)
	original := fixtureJPEG(t, base, 92)
	want := mustProcess(t, uploadListing, original).perceptualHash
	if want == "" || !moderation.InformativePerceptualHash(want) {
		t.Fatalf("textured fixture produced no informative hash: %q", want)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	crop := image.NewRGBA(image.Rect(0, 0, 1188, 891)) // 1% off the right and bottom edges
	draw.Copy(crop, image.Point{}, decoded, crop.Rect, draw.Src, nil)
	for name, data := range map[string][]byte{
		"recompress q92 to q70": fixtureJPEG(t, decoded, 70),
		"resize 1000x750":       fixtureJPEG(t, scaledFixture(decoded, 1000, 750), 85),
		"resize 640x480":        fixtureJPEG(t, scaledFixture(decoded, 640, 480), 85),
		"crop 1%":               fixtureJPEG(t, crop, 90),
	} {
		got := mustProcess(t, uploadListing, data).perceptualHash
		if d := hashDistance(t, want, got); d > 5 || !moderation.ImageHashesMatch(want, got, 5) {
			t.Errorf("%s: distance %d (%s vs %s)", name, d, want, got)
		}
	}
	// A horizontal mirror reverses and complements each row's gradient bits.
	// The stored hash is unchanged; only the comparison also tries the mirror.
	mirror := image.NewRGBA(base.Rect)
	for y := range 900 {
		for x := range 1200 {
			mirror.SetRGBA(x, y, base.RGBAAt(1199-x, y))
		}
	}
	mirrored := mustProcess(t, uploadListing, fixtureJPEG(t, mirror, 92)).perceptualHash
	if hashDistance(t, want, mirrored) <= 5 || !moderation.ImageHashesMatch(want, mirrored, 5) || !moderation.ImageHashesMatch(mirrored, want, 5) {
		t.Errorf("mirrored picture: forward distance %d, mirror comparison missed (%s vs %s)", hashDistance(t, want, mirrored), want, mirrored)
	}
	hashes := []string{want}
	for seed := int64(1); seed <= 40; seed++ {
		h := mustProcess(t, uploadListing, fixtureJPEG(t, texturedFixture(1000+seed, 320, 240), 90)).perceptualHash
		if !moderation.InformativePerceptualHash(h) {
			t.Fatalf("unrelated fixture %d produced no informative hash", seed)
		}
		hashes = append(hashes, h)
	}
	for i := range hashes {
		for j := i + 1; j < len(hashes); j++ {
			if moderation.ImageHashesMatch(hashes[i], hashes[j], 5) {
				t.Errorf("unrelated fixtures %d and %d matched: %s %s", i, j, hashes[i], hashes[j])
			}
		}
	}
}
