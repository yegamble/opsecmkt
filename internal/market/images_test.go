package market

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A-186: the caps before the scan (C6) and the decode and re-encode after it (D10). No database needed.

func refusalCode(t *testing.T, err error) int {
	t.Helper()
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want an *httpError", err)
	}
	return he.Code
}

// assertCleanJPEG checks stored bytes: a JPEG from image/jpeg with no APPn or COM segment, a single SOI, and
// nothing after EOI, carrying no planted fixture string.
func assertCleanJPEG(t *testing.T, name string, b []byte) {
	t.Helper()
	if !bytes.HasPrefix(b, jpegMagic) {
		t.Fatalf("%s: does not start FF D8 FF", name)
	}
	if bytes.Contains(b, []byte("FIXTURE")) || bytes.Contains(b, []byte("PK\x03\x04")) || bytes.Contains(b, []byte(fakeClamdMarker)) {
		t.Fatalf("%s: carries planted data", name)
	}
	if i := bytes.Index(b, []byte{0xFF, 0xD9}); i != len(b)-2 {
		t.Fatalf("%s: EOI at %d of %d (data after EOI)", name, i, len(b))
	}
	if bytes.Count(b, []byte{0xFF, 0xD8}) != 1 {
		t.Fatalf("%s: more than one SOI (embedded JPEG)", name)
	}
	for i := 2; i+4 <= len(b); {
		m := b[i+1]
		if m == 0xDA {
			return
		}
		if m >= 0xE0 && m <= 0xEF || m == 0xFE {
			t.Fatalf("%s: segment FF %X kept", name, m)
		}
		i += 2 + int(binary.BigEndian.Uint16(b[i+2:]))
	}
	t.Fatalf("%s: no scan", name)
}

// The package never reaches image's format registry: jpeg.Decode and png.Decode are chosen by magic bytes, and
// no other decoder is imported.
func TestNoImageDecodeRegistry(t *testing.T) {
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) < 50 {
		t.Fatalf("package files: %d, %v", len(files), err)
	}
	banned := regexp.MustCompile(`image\.Decode(Config)?\(|image\.RegisterFormat|"image/gif"|"golang.org/x/image/(webp|bmp|tiff|vp8|vp8l|ccitt)"`)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := banned.Find(src); m != nil {
			t.Errorf("%s uses %s", name, m)
		}
	}
}

// The type comes from the first bytes alone; everything but JPEG and PNG is refused with 415.
func TestTypeFromMagicOnly(t *testing.T) {
	for name, data := range map[string][]byte{
		"gif":   []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"bmp":   append([]byte("BM"), make([]byte, 64)...),
		"webp":  []byte("RIFF\x24\x00\x00\x00WEBPVP8 "),
		"heic":  []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"),
		"tiff":  []byte("II*\x00\x08\x00\x00\x00"),
		"text":  []byte("FIXTURE plain text named photo.jpg"),
		"empty": nil,
		// JPEG and PNG bytes behind a prefix are not JPEG or PNG.
		"prefixed-jpeg": append([]byte{0}, fixtureJPEG(t, fixturePicture(8, 8), 80)...),
		"prefixed-png":  append([]byte(" "), fixturePNG(t, fixturePicture(8, 8))...),
	} {
		if _, err := inspectImage(data); refusalCode(t, err) != http.StatusUnsupportedMediaType {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, data := range map[string][]byte{"jpeg": fixtureJPEG(t, fixturePicture(8, 8), 80), "png": fixturePNG(t, fixturePicture(8, 8))} {
		info, err := inspectImage(data)
		if err != nil || string(info.format) != strings.ToUpper(name) {
			t.Errorf("%s: %+v %v", name, info, err)
		}
	}
}

func TestSVGRefused(t *testing.T) {
	for _, svg := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><script>FIXTURE</script></svg>`,
		`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"\xEF\xBB\xBF<svg/>",
	} {
		if _, err := inspectImage([]byte(svg)); refusalCode(t, err) != http.StatusUnsupportedMediaType {
			t.Errorf("%q: %v", svg, err)
		}
	}
}

// C6: a 4097×1 PNG is refused from its IHDR alone: the IDAT that follows is garbage and never read.
func TestDecodeConfigCapBeforeScan(t *testing.T) {
	for _, c := range []struct {
		w, h uint32
		want string
	}{
		{4097, 1, "4097×1 pixels"},
		{1, 4097, "1×4097 pixels"},
		{3465, 3464, "3465×3464 pixels"}, // 12 002 760 px
		{1 << 31, 1, "1073741824×1 pixels"},
	} {
		_, err := inspectImage(pngHeader(c.w, c.h, 8, 2, 0))
		if refusalCode(t, err) != 422 || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "4096 pixels") || !strings.Contains(err.Error(), "12 megapixels") {
			t.Errorf("%dx%d: %v", c.w, c.h, err)
		}
	}
	if _, err := inspectImage(jpegHeader(0xC0, 4097, 16, 3)); refusalCode(t, err) != 422 || !strings.Contains(err.Error(), "4097×16") {
		t.Errorf("wide JPEG: %v", err)
	}
	// At the limits: 4096 on a side and exactly 12 000 000 px pass the caps.
	for _, data := range [][]byte{pngHeader(4096, 16, 8, 2, 0), jpegHeader(0xC0, 4000, 3000, 3)} {
		if _, err := inspectImage(data); err != nil {
			t.Errorf("at the limit: %v", err)
		}
	}
	// Over 4 MB is refused before anything is read.
	if _, err := inspectImage(make([]byte, uploadCap+1)); refusalCode(t, err) != 413 || !strings.Contains(err.Error(), "4 MB") {
		t.Errorf("oversized: %v", err)
	}
}

// C6: a 4-component JPEG (CMYK or YCCK), baseline or progressive, is refused from its frame header.
func TestRejectCMYK(t *testing.T) {
	for _, sof := range []byte{0xC0, 0xC1, 0xC2} {
		_, err := inspectImage(jpegHeader(sof, 16, 16, 4))
		if refusalCode(t, err) != 415 || !strings.Contains(err.Error(), "CMYK") {
			t.Errorf("SOF %X: %v", sof, err)
		}
	}
}

// C6: the decode allocation is estimated from the colour model; a progressive JPEG counts 12 B/px, an
// interlaced PNG twice.
func TestProgressiveAllocationCap(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"baseline 12 MP YCbCr (36 MB)", jpegHeader(0xC0, 4000, 3000, 3), true},
		{"progressive 3 MP (36 MB)", jpegHeader(0xC2, 2000, 1500, 3), true},
		{"progressive 6 MP (72 MB)", jpegHeader(0xC2, 3000, 2000, 3), false},
		{"PNG 16-bit RGBA 7.8 MP (63 MB)", pngHeader(2800, 2800, 16, 6, 0), true},
		{"PNG 16-bit RGBA 9 MP (72 MB)", pngHeader(3000, 3000, 16, 6, 0), false},
		{"PNG 8-bit RGBA 12 MP (48 MB)", pngHeader(4000, 3000, 8, 6, 0), true},
		{"PNG 8-bit RGBA interlaced 9 MP (72 MB)", pngHeader(3000, 3000, 8, 6, 1), false},
	} {
		info, err := inspectImage(c.data)
		if c.ok && err != nil {
			t.Errorf("%s: refused %v", c.name, err)
		}
		if !c.ok && (err == nil || refusalCode(t, err) != 422 || !strings.Contains(err.Error(), "64 MB")) {
			t.Errorf("%s: %+v %v", c.name, info, err)
		}
	}
}

// A panic in the decoder is recovered and refused.
func TestDecodePanicRecovered(t *testing.T) {
	data := fixtureJPEG(t, fixturePicture(32, 32), 80)
	info, err := inspectImage(data)
	if err != nil {
		t.Fatal(err)
	}
	u := scanFixture(t, uploadListing, data)
	old := decodeJPEG
	decodeJPEG = func(io.Reader) (image.Image, error) { panic("FIXTURE decoder bug") }
	t.Cleanup(func() { decodeJPEG = old })
	if _, err = reencodeImage(u, info, uploadListing); refusalCode(t, err) != 422 {
		t.Fatalf("panic: %v", err)
	}
	// Bounds that disagree with the config are refused too.
	decodeJPEG = func(io.Reader) (image.Image, error) { return image.NewRGBA(image.Rect(0, 0, 31, 32)), nil }
	if _, err = reencodeImage(u, info, uploadListing); refusalCode(t, err) != 422 {
		t.Fatalf("bounds: %v", err)
	}
}

// EXIF (GPS, camera, embedded thumbnail), XMP, IPTC, ICC and comments are gone from both stored JPEGs.
func TestReencodeStripsExifXmpIccComThumbnail(t *testing.T) {
	data := metadataJPEG(t, 400, 300)
	for _, planted := range []string{"FIXTURE-EXIF-CAMERA", "FIXTURE-EXIF-GPS", "FIXTURE-EXIF-THUMB", "FIXTURE-XMP-GPS", "FIXTURE-ICC-PROFILE", "FIXTURE-IPTC-BYLINE", "FIXTURE-JPEG-COMMENT"} {
		if !bytes.Contains(data, []byte(planted)) {
			t.Fatalf("fixture lacks %s", planted)
		}
	}
	for _, kind := range []uploadKind{uploadListing, uploadAvatar} {
		out := mustProcess(t, kind, data)
		assertCleanJPEG(t, string(kind)+" full", out.full)
		assertCleanJPEG(t, string(kind)+" thumb", out.thumb)
	}
}

func TestPngAncillaryChunksDropped(t *testing.T) {
	data := ancillaryPNG(t, 300, 200)
	if _, err := decodeFixturePNG(data); err != nil {
		t.Fatalf("fixture PNG does not decode: %v", err)
	}
	out := mustProcess(t, uploadListing, data)
	assertCleanJPEG(t, "full", out.full)
	assertCleanJPEG(t, "thumb", out.thumb)
	for _, chunk := range []string{"tEXt", "iTXt", "zTXt", "eXIf", "tIME", "iCCP"} {
		if bytes.Contains(out.full, []byte(chunk)) {
			t.Errorf("stored JPEG mentions %s", chunk)
		}
	}
}

// decodeFixturePNG decodes a fixture with the real decoder (the fixture must be a valid PNG to prove anything).
func decodeFixturePNG(b []byte) (image.Image, error) { return decodePNG(bytes.NewReader(b)) }

// A ZIP (or anything) appended after the image is not stored.
func TestPolyglotTrailerDropped(t *testing.T) {
	trailer := zipTrailer(t)
	for name, data := range map[string][]byte{
		"jpeg": append(fixtureJPEG(t, fixturePicture(200, 150), 85), trailer...),
		"png":  append(fixturePNG(t, fixturePicture(200, 150)), trailer...),
	} {
		out := mustProcess(t, uploadListing, data)
		assertCleanJPEG(t, name, out.full)
		assertCleanJPEG(t, name+" thumb", out.thumb)
	}
}

// A JPEG that already looks like the output (image/jpeg's own q82, no metadata, within 1200 px) is still
// decoded and re-encoded, never passed through.
func TestConformingInputIsStillReencoded(t *testing.T) {
	data := encodeJPEG(fixturePicture(640, 480), photoQuality)
	out := mustProcess(t, uploadListing, data)
	if bytes.Equal(out.full, data) {
		t.Fatal("conforming input stored byte for byte")
	}
	if out.width != 640 || out.height != 480 {
		t.Fatalf("size %dx%d", out.width, out.height)
	}
}

// A message hidden in the least significant bits of the pixels does not survive the re-encode.
func TestLsbPlaneNotRecoverable(t *testing.T) {
	img := fixturePicture(256, 256)
	rng := rand.New(rand.NewPCG(1, 86))
	bits := make([]byte, 256*256)
	for i := range bits {
		bits[i] = byte(rng.IntN(2))
		img.Pix[4*i] = img.Pix[4*i]&^1 | bits[i]
	}
	out := mustProcess(t, uploadListing, fixturePNG(t, img))
	dec := decodeOutput(t, out.full)
	if dec.Bounds().Dx() != 256 || dec.Bounds().Dy() != 256 {
		t.Fatalf("size %v", dec.Bounds())
	}
	same := 0
	for i := range bits {
		r, _, _, _ := dec.At(i%256, i/256).RGBA()
		if byte(r>>8)&1 == bits[i] {
			same++
		}
	}
	if frac := float64(same) / float64(len(bits)); frac < 0.45 || frac > 0.55 {
		t.Fatalf("hidden LSB plane recovered at %.3f (chance is 0.5)", frac)
	}
}

// The eight EXIF orientations are applied to the pixels (the stored JPEG has no EXIF to rotate it by).
func TestOrientationApplied(t *testing.T) {
	R, G, B, W := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}
	// Stored: R G / B W. Shown: top-left, top-right, bottom-left, bottom-right.
	want := map[int][4]color.RGBA{
		1: {R, G, B, W}, 2: {G, R, W, B}, 3: {W, B, G, R}, 4: {B, W, R, G},
		5: {R, B, G, W}, 6: {B, R, W, G}, 7: {W, G, B, R}, 8: {G, W, R, B},
	}
	near := func(a color.Color, b color.RGBA) bool {
		r, g, bb, _ := a.RGBA()
		d := func(x uint32, y uint8) bool { return int(x>>8)-int(y) < 70 && int(y)-int(x>>8) < 70 }
		return d(r, b.R) && d(g, b.G) && d(bb, b.B)
	}
	for o := 1; o <= 8; o++ {
		data := quadrantJPEG(t, o)
		info, err := inspectImage(data)
		if err != nil || info.orientation != o {
			t.Fatalf("orientation %d: read %d, %v", o, info.orientation, err)
		}
		out := mustProcess(t, uploadListing, data)
		img := decodeOutput(t, out.full)
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		if (o >= 5) != (w == 32 && h == 64) || (o < 5) != (w == 64 && h == 32) || out.width != w || out.height != h {
			t.Fatalf("orientation %d: shown %dx%d (recorded %dx%d)", o, w, h, out.width, out.height)
		}
		got := [4]color.Color{img.At(w/4, h/4), img.At(3*w/4, h/4), img.At(w/4, 3*h/4), img.At(3*w/4, 3*h/4)}
		for q := range 4 {
			if !near(got[q], want[o][q]) {
				t.Errorf("orientation %d quadrant %d: %v, want %v", o, q, got[q], want[o][q])
			}
		}
	}
}

// Nothing is upscaled: a small picture keeps its size; its thumbnail sits at that size centred in 320×240.
func TestNeverUpscales(t *testing.T) {
	data := fixtureJPEG(t, fixturePicture(100, 80), 90)
	out := mustProcess(t, uploadListing, data)
	if out.width != 100 || out.height != 80 {
		t.Fatalf("full %dx%d", out.width, out.height)
	}
	thumb := decodeOutput(t, out.thumb)
	if thumb.Bounds().Dx() != 320 || thumb.Bounds().Dy() != 240 {
		t.Fatalf("thumb %v", thumb.Bounds())
	}
	if r, g, b, _ := thumb.At(20, 20).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("letterbox is not white: %d %d %d", r>>8, g>>8, b>>8)
	}
	if r, _, _, _ := thumb.At(160, 120).RGBA(); r>>8 > 200 {
		t.Fatal("thumbnail centre is not the picture")
	}
	avatar := mustProcess(t, uploadAvatar, data)
	if avatar.width != 80 || avatar.height != 80 {
		t.Fatalf("avatar %dx%d", avatar.width, avatar.height)
	}
	if b := decodeOutput(t, avatar.thumb).Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("avatar thumb %v", b)
	}
	tiny := mustProcess(t, uploadAvatar, fixtureJPEG(t, fixturePicture(40, 50), 90))
	if b := decodeOutput(t, tiny.thumb).Bounds(); tiny.width != 40 || b.Dx() != 40 || b.Dy() != 40 {
		t.Fatalf("tiny avatar %d, thumb %v", tiny.width, b)
	}
}

// Thumbnails are exactly 320×240 (letterboxed) and within the 32 KiB budget on the fixtures; the full photo fits
// 1200×1200; an avatar is 256×256 and 64×64.
func TestThumbnailExactBoxAndBudget(t *testing.T) {
	for _, c := range []struct {
		name   string
		data   []byte
		fw, fh int
	}{
		{"landscape JPEG", fixtureJPEG(t, fixturePicture(1600, 1200), 90), 1200, 900},
		{"portrait PNG", fixturePNG(t, fixturePicture(900, 1600)), 675, 1200},
		{"panorama JPEG", fixtureJPEG(t, fixturePicture(4000, 500), 90), 1200, 150},
		{"alpha PNG", fixturePNG(t, alphaPicture(640, 480)), 640, 480},
	} {
		out := mustProcess(t, uploadListing, c.data)
		full, thumb := decodeOutput(t, out.full), decodeOutput(t, out.thumb)
		if full.Bounds().Dx() != c.fw || full.Bounds().Dy() != c.fh || out.width != c.fw || out.height != c.fh {
			t.Errorf("%s: full %v (recorded %dx%d), want %dx%d", c.name, full.Bounds(), out.width, out.height, c.fw, c.fh)
		}
		if thumb.Bounds() != image.Rect(0, 0, 320, 240) {
			t.Errorf("%s: thumb %v", c.name, thumb.Bounds())
		}
		if len(out.thumb) > 32<<10 {
			t.Errorf("%s: thumb %d bytes, budget 32 KiB", c.name, len(out.thumb))
		}
	}
	avatar := mustProcess(t, uploadAvatar, fixtureJPEG(t, fixturePicture(1600, 1200), 90))
	if avatar.width != 256 || avatar.height != 256 || decodeOutput(t, avatar.full).Bounds() != image.Rect(0, 0, 256, 256) || decodeOutput(t, avatar.thumb).Bounds() != image.Rect(0, 0, 64, 64) {
		t.Fatalf("avatar %dx%d", avatar.width, avatar.height)
	}
}

// alphaPicture is fixturePicture with a transparent left half, which must come out white.
func alphaPicture(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	src := fixturePicture(w, h)
	copy(img.Pix, src.Pix)
	for y := range h {
		for x := range w / 2 {
			img.Pix[img.PixOffset(x, y)+3] = 0
		}
	}
	return img
}

func TestAlphaFlattenedOnWhite(t *testing.T) {
	out := mustProcess(t, uploadListing, fixturePNG(t, alphaPicture(640, 480)))
	img := decodeOutput(t, out.full)
	if r, g, b, _ := img.At(100, 240).RGBA(); r>>8 < 245 || g>>8 < 245 || b>>8 < 245 {
		t.Fatalf("transparent area is %d %d %d, want white", r>>8, g>>8, b>>8)
	}
}

// Whatever bytes arrive, the caps and the re-encoder refuse them or store a clean JPEG; they never panic.
func FuzzDecodeReencode(f *testing.F) {
	jpg := encodeJPEG(fixturePicture(24, 16), 80)
	pngData := fixturePNGBytes(fixturePicture(24, 16))
	f.Add(jpg)
	f.Add(pngData)
	f.Add(jpg[:len(jpg)/2])
	f.Add(pngData[:len(pngData)/2])
	f.Add(jpegOrientationSeed())
	f.Add(jpegHeader(0xC2, 16, 16, 3))
	f.Add(pngHeader(16, 16, 16, 6, 1))
	f.Add([]byte("\xFF\xD8\xFF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := inspectImage(data)
		if err != nil {
			refusalCode(t, err)
			return
		}
		if info.width*info.height > 1<<20 {
			return // valid but slow to fuzz
		}
		out, err := reencodeImage(scanFixture(t, uploadListing, data), info, uploadListing)
		if err != nil {
			refusalCode(t, err)
			return
		}
		assertCleanJPEG(t, "full", out.full)
		assertCleanJPEG(t, "thumb", out.thumb)
	})
}

func fixturePNGBytes(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func jpegOrientationSeed() []byte {
	return withJPEGSegments(encodeJPEG(fixturePicture(8, 6), 80), jpegSegment(0xE1, append([]byte("Exif\x00\x00"), exifTIFF(6, nil)...)))
}
