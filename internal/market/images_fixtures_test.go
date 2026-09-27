package market

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// A-186 image fixtures. The pre-commit hook refuses binaries, so every JPEG and PNG a test uploads is built
// here at run time: pictures with metadata (EXIF with GPS and an embedded thumbnail, XMP, IPTC, ICC, comments),
// PNG ancillary chunks, polyglot trailers, a hidden LSB plane, the eight EXIF orientations, the CI scanner
// marker, and hand-made headers for the caps. Every planted string starts "FIXTURE-" so a test can look for
// any of them in what was stored.

// fixturePicture is a w×h photo-like picture: smooth gradients with a mild texture, so it compresses like a
// photograph rather than like noise or a flat colour.
func fixturePicture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			t := uint8((x*7 + y*13) % 17)
			p := img.PixOffset(x, y)
			img.Pix[p] = uint8(40 + 160*x/max(1, w-1) + int(t))
			img.Pix[p+1] = uint8(60 + 140*y/max(1, h-1) + int(t))
			img.Pix[p+2] = uint8(120 + 80*(x+y)/max(1, w+h-2) - int(t))
			img.Pix[p+3] = 0xFF
		}
	}
	return img
}

func fixtureJPEG(t testing.TB, img image.Image, quality int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixturePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// jpegSegment is marker's segment with payload (FF marker, 16-bit length, payload).
func jpegSegment(marker byte, payload []byte) []byte {
	b := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(payload)+2))
	return append(b, payload...)
}

// withJPEGSegments inserts segs right after the SOI of jpg.
func withJPEGSegments(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte(nil), jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

// exifTIFF is a big-endian TIFF structure: IFD0 with Make, Orientation and a GPS IFD pointer; the GPS IFD with
// a latitude and a processing-method string; IFD1 pointing at an embedded JPEG thumbnail.
func exifTIFF(orientation int, thumb []byte) []byte {
	var b bytes.Buffer
	be := binary.BigEndian
	u16 := func(v uint16) { binary.Write(&b, be, v) }
	u32 := func(v uint32) { binary.Write(&b, be, v) }
	entry := func(tag, typ uint16, count, value uint32) { u16(tag); u16(typ); u32(count); u32(value) }
	short := func(tag, v uint16) { u16(tag); u16(3); u32(1); u16(v); u16(0) }
	make_ := []byte("FIXTURE-EXIF-CAMERA\x00")
	method := []byte("ASCII\x00\x00\x00FIXTURE-EXIF-GPS")
	const ifd0 = 8
	const ifd0Size = 2 + 3*12 + 4
	makeAt := ifd0 + ifd0Size
	gpsAt := makeAt + len(make_)
	const gpsSize = 2 + 3*12 + 4
	latAt := gpsAt + gpsSize
	methodAt := latAt + 24
	ifd1At := methodAt + len(method)
	const ifd1Size = 2 + 2*12 + 4
	thumbAt := ifd1At + ifd1Size

	b.WriteString("MM\x00*")
	u32(ifd0)
	u16(3)
	entry(0x010F, 2, uint32(len(make_)), uint32(makeAt)) // Make
	short(0x0112, uint16(orientation))                   // Orientation
	entry(0x8825, 4, 1, uint32(gpsAt))                   // GPSInfo
	u32(uint32(ifd1At))
	b.Write(make_)
	u16(3)
	u16(0x0001) // GPSLatitudeRef "N"
	u16(2)
	u32(2)
	b.Write([]byte{'N', 0, 0, 0})
	entry(0x0002, 5, 3, uint32(latAt))                      // GPSLatitude
	entry(0x001B, 7, uint32(len(method)), uint32(methodAt)) // GPSProcessingMethod
	u32(0)
	for _, v := range []uint32{51, 1, 30, 1, 2667, 100} {
		u32(v)
	}
	b.Write(method)
	u16(2)
	entry(0x0201, 4, 1, uint32(thumbAt))    // JPEGInterchangeFormat
	entry(0x0202, 4, 1, uint32(len(thumb))) // JPEGInterchangeFormatLength
	u32(0)
	b.Write(thumb)
	return b.Bytes()
}

// exifThumbnail is a tiny JPEG with a comment, as a camera embeds in its EXIF.
func exifThumbnail(t testing.TB) []byte {
	return withJPEGSegments(fixtureJPEG(t, fixturePicture(16, 12), 50), jpegSegment(0xFE, []byte("FIXTURE-EXIF-THUMB")))
}

// exifSegment is an APP1 Exif segment carrying orientation, GPS and an embedded thumbnail.
func exifSegment(t testing.TB, orientation int) []byte {
	return jpegSegment(0xE1, append([]byte("Exif\x00\x00"), exifTIFF(orientation, exifThumbnail(t))...))
}

// metadataJPEG is a w×h JPEG carrying EXIF (GPS, camera, embedded thumbnail), XMP, IPTC, an ICC profile and a
// comment.
func metadataJPEG(t testing.TB, w, h int) []byte {
	return withJPEGSegments(fixtureJPEG(t, fixturePicture(w, h), 90),
		exifSegment(t, 1),
		jpegSegment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta><rdf:Description exif:GPSLatitude=\"FIXTURE-XMP-GPS\"/></x:xmpmeta>")),
		jpegSegment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), []byte("FIXTURE-ICC-PROFILE")...)),
		jpegSegment(0xED, []byte("Photoshop 3.0\x008BIM\x04\x04\x00\x00\x00\x00\x00\x14FIXTURE-IPTC-BYLINE")),
		jpegSegment(0xFE, []byte("FIXTURE-JPEG-COMMENT")))
}

// pngChunk is one PNG chunk with its CRC.
func pngChunk(kind string, body []byte) []byte {
	b := make([]byte, 8, 12+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(body)))
	copy(b[4:], kind)
	b = append(b, body...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b[4:]))
}

// withPNGChunks inserts chunks right after the IHDR of p.
func withPNGChunks(p []byte, chunks ...[]byte) []byte {
	at := len(pngMagic) + 12 + 13
	out := append([]byte(nil), p[:at]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[at:]...)
}

func zlibBytes(b []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(b)
	w.Close()
	return z.Bytes()
}

// ancillaryPNG is a w×h PNG carrying tEXt, iTXt, zTXt, eXIf, tIME and iCCP chunks.
func ancillaryPNG(t testing.TB, w, h int) []byte {
	return withPNGChunks(fixturePNG(t, fixturePicture(w, h)),
		pngChunk("tEXt", []byte("Author\x00FIXTURE-PNG-TEXT")),
		pngChunk("iTXt", []byte("Comment\x00\x00\x00en\x00\x00FIXTURE-PNG-ITXT")),
		pngChunk("zTXt", append([]byte("Software\x00\x00"), zlibBytes([]byte("FIXTURE-PNG-ZTXT"))...)),
		pngChunk("eXIf", exifTIFF(1, exifThumbnail(t))),
		pngChunk("tIME", []byte{0x07, 0xEA, 9, 27, 12, 0, 0}),
		pngChunk("iCCP", append([]byte("FIXTURE-ICC\x00\x00"), zlibBytes([]byte("FIXTURE-PNG-ICC-PROFILE"))...)))
}

// markerPNG is a PNG carrying fakeClamdMarker in a tEXt chunk: it passes every cap and the fake clamd (like
// A-192's CI signature) reports it FOUND.
func markerPNG(t testing.TB) []byte {
	return withPNGChunks(fixturePNG(t, fixturePicture(64, 48)), pngChunk("tEXt", []byte("Comment\x00"+fakeClamdMarker)))
}

// zipTrailer is a ZIP archive to append after an image (a polyglot).
func zipTrailer(t testing.TB) []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, err := z.Create("FIXTURE-ZIP-NAME.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("FIXTURE-ZIP-CONTENT"))
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// quadrantJPEG is a 64×32 JPEG stored red | green over blue | white, with EXIF orientation o.
func quadrantJPEG(t testing.TB, o int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := range 32 {
		for x := range 64 {
			c := [4]color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 255, 255}}[2*(y/16)+x/32]
			img.SetRGBA(x, y, c)
		}
	}
	return withJPEGSegments(fixtureJPEG(t, img, 95), exifSegment(t, o))
}

// pngHeader is a PNG signature and IHDR claiming w×h (bit depth, colour type, interlace as given), followed by
// an IDAT of garbage: anything that reads past IHDR fails.
func pngHeader(w, h uint32, depth, colorType, interlace byte) []byte {
	ihdr := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, w), h)
	ihdr = append(ihdr, depth, colorType, 0, 0, interlace)
	out := append([]byte(nil), pngMagic...)
	out = append(out, pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IDAT", []byte("FIXTURE-not-zlib-data"))...)
	return append(out, pngChunk("IEND", nil)...)
}

// jpegHeader is SOI, a frame header (marker sof, w×h, comps components), a scan header and EOI: enough for
// DecodeConfig, nothing to decode.
func jpegHeader(sof byte, w, h uint16, comps int) []byte {
	frame := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(comps)}
	scan := []byte{byte(comps)}
	for c := 1; c <= comps; c++ {
		samp := byte(0x11)
		if c == 1 && comps > 1 {
			samp = 0x22
		}
		frame = append(frame, byte(c), samp, 0)
		scan = append(scan, byte(c), 0)
	}
	scan = append(scan, 0, 63, 0)
	out := []byte{0xFF, 0xD8}
	out = append(out, jpegSegment(sof, frame)...)
	out = append(out, jpegSegment(0xDA, scan)...)
	return append(out, 0xFF, 0xD9)
}

// scanFixture scans data with a fresh fake clamd (answering OK unless the marker is present) and returns the
// scannedUpload: only the scanner builds one.
func scanFixture(t *testing.T, kind uploadKind, data []byte) scannedUpload {
	t.Helper()
	s := readyScanner(t, newFakeClamd(t))
	u, err := s.Scan(context.Background(), kind, data)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return u
}

// processFixture runs data through the caps, the scan and the re-encoder as the upload pipeline does.
func processFixture(t *testing.T, kind uploadKind, data []byte) (encodedImage, error) {
	t.Helper()
	info, err := inspectImage(data)
	if err != nil {
		return encodedImage{}, err
	}
	return reencodeImage(scanFixture(t, kind, data), info, kind)
}

// mustProcess is processFixture that fails the test on a refusal.
func mustProcess(t *testing.T, kind uploadKind, data []byte) encodedImage {
	t.Helper()
	out, err := processFixture(t, kind, data)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	return out
}

// decodeOutput decodes a stored JPEG.
func decodeOutput(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("stored bytes are not a JPEG: %v", err)
	}
	return img
}
