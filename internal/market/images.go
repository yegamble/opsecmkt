package market

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"

	"golang.org/x/image/draw"
)

// A-186 (war room 2026-09-27, rulings C6 and D10): what happens to the bytes of a photo or avatar upload.
//
// Before the scan (inspectImage), with nothing decoded: the byte cap; the type from the magic bytes only (JPEG
// FF D8 FF, PNG 89 50 4E 47 0D 0A 1A 0A; the part's file name and Content-Type are never read); a walk of the
// segment (JPEG) or chunk (PNG) headers up to the image data, which also yields the EXIF Orientation; then
// jpeg.DecodeConfig or png.DecodeConfig, which must agree with the walk. Refused: either side over 4096 px,
// more than 4096×3072 px, a 4-component (CMYK or YCCK) JPEG, and an estimated decode allocation over 64 MiB
// (bytesPerPixel: a JPEG's pixels rounded up to whole MCUs × its planes, plus, when progressive, 256 bytes for each
// 8×8 coefficient block of every component at its own sampling, and 4 B/px for the RGBA copy of an RGB JPEG; a PNG's pixels × its colour model's size, twice when
// interlaced), and a JPEG of more than 100 scans. Only bytes passing every cap reach clamd.
//
// After a clean scan (reencodeImage, which takes only a scannedUpload): jpeg.Decode or png.Decode chosen by
// the magic bytes (the package never calls image.Decode, so no other registered format is reachable); bounds
// equal to the config; resampled with golang.org/x/image/draw to the D10 boxes with alpha flattened on white,
// never upscaled; the EXIF orientation applied to the pixels; encoded by image/jpeg, which writes no APPn or
// COM segment and nothing after EOI. So nothing of the upload but its picture survives: no EXIF, XMP, IPTC,
// ICC profile, comment, embedded thumbnail, PNG text chunk or trailing data, and no bit-exact pixel either. A
// panic in any of it is recovered and refused (422).

const (
	uploadCap      = 4 << 20     // bytes of one photo or avatar file
	maxImageSide   = 4096        // pixels on either side
	maxImagePixels = 4096 * 3072 // width × height: about 12.6 megapixels, so a 4032×3024 phone photo fits
	maxDecodeBytes = 64 << 20    // estimated decode allocation

	photoLongEdge           = 1200 // full photo: fits a 1200×1200 box
	photoQuality            = 82
	thumbWidth, thumbHeight = 320, 240 // listing thumbnail: letterboxed to exactly this
	thumbQuality            = 70
	avatarSide              = 256 // avatar: centre-cropped square
	avatarThumbSide         = 64
	maxFullBytes            = 2 << 20   // migration 063's CHECK on images.full
	maxThumbBytes           = 256 << 10 // and on images.thumb
	maxHeaderSegments       = 256       // JPEG segments or PNG chunks walked before the image data
	maxJPEGScans            = 100       // encoders write about 10; libjpeg-turbo suggests a limit of a few hundred
)

type imageFormat string

const (
	formatJPEG imageFormat = "JPEG"
	formatPNG  imageFormat = "PNG"
)

var (
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
)

// imageInfo is what inspectImage learned without decoding: the stored size (before orientation), the EXIF
// orientation (1–8) and the estimated decode allocation.
type imageInfo struct {
	format        imageFormat
	width, height int
	orientation   int
	progressive   bool // JPEG SOF2; PNG interlaced
	components    int  // JPEG: 1 or 3 (4 is refused)
	mcuW, mcuH    int  // JPEG: the decoder rounds the image up to whole MCUs of this many pixels
	mcuBlocks     int  // JPEG: 8×8 blocks per MCU over all components (the sum of each one's h×v sampling)
	decodeBytes   int64
}

// The uploader's copy for each refusal before the scan; none quotes the file name or anything else sent.
var (
	errImageTooLarge   = &httpError{Code: http.StatusRequestEntityTooLarge, Msg: "Photo not saved: the file is larger than 4 MB, the most accepted. Nothing was stored."}
	errImageType       = &httpError{Code: http.StatusUnsupportedMediaType, Msg: "Photo not saved: only JPEG and PNG files are accepted. Nothing was stored."}
	errImageCMYK       = &httpError{Code: http.StatusUnsupportedMediaType, Msg: "Photo not saved: CMYK JPEGs are not accepted. Save the picture as an RGB JPEG or a PNG and upload that. Nothing was stored."}
	errImageUnreadable = &httpError{Code: http.StatusUnprocessableEntity, Msg: "Photo not saved: the file is not a readable JPEG or PNG. Nothing was stored."}
	errImageMemory     = &httpError{Code: http.StatusUnprocessableEntity, Msg: "Photo not saved: this picture would need more than 64 MB of memory to process (a progressive JPEG needs up to five times as much as a standard one, and a 16-bit PNG more than twice). Save it as a standard JPEG or at a smaller size. Nothing was stored."}
	errImageScans      = &httpError{Code: http.StatusUnprocessableEntity, Msg: "Photo not saved: this JPEG has more than 100 progressive scans. Save it as a standard JPEG and upload that. Nothing was stored."}
	errImageDetailed   = &httpError{Code: http.StatusUnprocessableEntity, Msg: "Photo not saved: the re-encoded picture is too large to store (it is unusually detailed). Try a smaller or less detailed picture. Nothing was stored."}
)

func errImageDimensions(w, h int) error {
	return &httpError{Code: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("Photo not saved: the picture is %d×%d pixels; the limit is 4096 pixels on either side and 4096×3072 (about 12.6 megapixels) in all. Nothing was stored.", w, h)}
}

// inspectImage applies every cap that comes before the scan (C6). It decodes nothing.
func inspectImage(data []byte) (imageInfo, error) {
	var info imageInfo
	var err error
	switch {
	case len(data) > uploadCap:
		return info, errImageTooLarge
	case bytes.HasPrefix(data, jpegMagic):
		info, err = walkJPEG(data)
	case bytes.HasPrefix(data, pngMagic):
		info, err = walkPNG(data)
	default:
		return info, errImageType
	}
	if err != nil {
		return info, err
	}
	if info.width < 1 || info.height < 1 {
		return info, errImageUnreadable
	}
	if info.width > maxImageSide || info.height > maxImageSide || info.width*info.height > maxImagePixels {
		return info, errImageDimensions(info.width, info.height)
	}
	var cfg image.Config
	if info.format == formatJPEG {
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	} else {
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil || cfg.Width != info.width || cfg.Height != info.height {
		return info, errImageUnreadable
	}
	if cfg.ColorModel == color.CMYKModel {
		return info, errImageCMYK
	}
	// The walk and the decoder read the same frame header (walkJPEG accepts nothing the decoder would parse
	// differently); their component counts must agree.
	if info.format == formatJPEG && (info.components == 1) != (cfg.ColorModel == color.GrayModel) {
		return info, errImageUnreadable
	}
	w, h := int64(info.width), int64(info.height)
	if info.format == formatJPEG {
		w, h = roundUp(w, int64(info.mcuW)), roundUp(h, int64(info.mcuH))
	}
	info.decodeBytes = w * h * bytesPerPixel(info, cfg.ColorModel)
	if info.format == formatJPEG && info.progressive {
		// A progressive JPEG also keeps every coefficient block until the last scan: 64 int32s (256 bytes) per
		// 8×8 block, mcuBlocks of them per MCU, as image/jpeg allocates them.
		info.decodeBytes += (w / int64(info.mcuW)) * (h / int64(info.mcuH)) * int64(info.mcuBlocks) * 256
	}
	if info.decodeBytes > maxDecodeBytes {
		return info, errImageMemory
	}
	return info, nil
}

// bytesPerPixel is the decoder's allocation per pixel for the colour model DecodeConfig reported (for a JPEG,
// per pixel of the image rounded up to whole MCUs). A JPEG decodes into planes of at most 1 (grey) or 3 (YCbCr
// or RGB) bytes a pixel; an RGB (not YCbCr) one is then copied to RGBA, 4 more. A progressive one's coefficient
// blocks (4 bytes a pixel of each component at its own sampling) are added by inspectImage.
// Measured with image/jpeg: progressive 4:4:4 15.07 B/px, 4:2:0 7.57, grey 5.03, RGB 19.05; baseline RGB 7.01,
// 4:2:0 1.52 (TestDecodeAllocationWithinEstimate). A PNG decodes into one image of its model's size, and an
// interlaced one (info.progressive, set by walkPNG) into one per pass as well, so twice.
func bytesPerPixel(info imageInfo, m color.Model) int64 {
	if info.format == formatJPEG {
		var n int64 = 3 // YCbCr planes
		switch {
		case m == color.GrayModel:
			n = 1
		case m == color.RGBAModel: // stored as RGB, not YCbCr: decoded into planes, then copied to RGBA
			n = 3 + 4
		}
		return n
	}
	var n int64
	switch m {
	case color.GrayModel:
		n = 1
	case color.Gray16Model:
		n = 2
	case color.RGBA64Model, color.NRGBA64Model:
		n = 8
	default:
		if _, paletted := m.(color.Palette); paletted {
			n = 1
		} else {
			n = 4 // RGBA, NRGBA
		}
	}
	if info.progressive { // Adam7: each pass is decoded into its own image before the merge
		n *= 2
	}
	return n
}

// roundUp rounds n up to a multiple of m.
func roundUp(n, m int64) int64 { return (n + m - 1) / m * m }

// walkJPEG reads the segment headers from SOI to the first SOS (bodies are skipped by length, at most
// maxHeaderSegments of them): the frame header gives the size, component count, sampling factors and whether the
// JPEG is progressive, and the first APP1 Exif segment its orientation. It accepts a strict subset of what
// image/jpeg parses, so both read the same frame header: only the segments the decoder parses by their length
// (SOF0, SOF1, SOF2, DHT, DQT, DRI, APPn, COM), restart markers and fill bytes. Stray bytes between segments
// and FF 00 (which the decoder skips), TEM, the other frame types, JPG, DAC and the reserved markers (which it
// refuses), a second frame header, a frame the decoder would refuse, a truncated segment or no frame header are
// unreadable.
func walkJPEG(data []byte) (imageInfo, error) {
	info := imageInfo{format: formatJPEG, orientation: 1}
	sof, exif := false, false
	i := 2
	for n := 0; n < maxHeaderSegments; n++ {
		if i+1 >= len(data) || data[i] != 0xFF {
			return info, errImageUnreadable
		}
		for i+1 < len(data) && data[i+1] == 0xFF { // fill bytes
			i++
		}
		if i+1 >= len(data) {
			return info, errImageUnreadable
		}
		m := data[i+1]
		switch {
		case m == 0xDA || m == 0xD9: // SOS, EOI
			if !sof {
				return info, errImageUnreadable
			}
			// From here on every FF xx is a marker (entropy-coded data stuffs FF as FF 00). image/jpeg has no
			// limit on scans, and each progressive scan walks every block, so a few bytes a scan could keep the
			// one decode slot busy for minutes.
			if bytes.Count(data[i:], []byte{0xFF, 0xDA}) > maxJPEGScans {
				return info, errImageScans
			}
			return info, nil
		case m >= 0xD0 && m <= 0xD7: // RSTn: no length; the decoder skips a stray one
			i += 2
			continue
		case !(m >= 0xC0 && m <= 0xC2 || m == 0xC4 || m == 0xDB || m == 0xDD || m >= 0xE0 && m <= 0xEF || m == 0xFE):
			return info, errImageUnreadable // FF 00, TEM, SOF3/5-7/9-11/13-15, JPG, DAC, DNL, DHP, EXP, JPGn, reserved
		}
		if i+4 > len(data) {
			return info, errImageUnreadable
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 || i+2+length > len(data) {
			return info, errImageUnreadable
		}
		seg := data[i+4 : i+2+length]
		switch {
		case m <= 0xC2: // SOF0, SOF1 (baseline, extended), SOF2 (progressive)
			if sof || len(seg) < 6 {
				return info, errImageUnreadable
			}
			sof = true
			info.height = int(binary.BigEndian.Uint16(seg[1:]))
			info.width = int(binary.BigEndian.Uint16(seg[3:]))
			info.progressive = m == 0xC2
			info.components = int(seg[5])
			if info.components == 4 {
				return info, errImageCMYK
			}
			if (info.components != 1 && info.components != 3) || len(seg) != 6+3*info.components {
				return info, errImageUnreadable
			}
			// The largest sampling factors set the MCU the decoder rounds the image up to (a grey image's is
			// always 8×8).
			maxH, maxV, blocks := 1, 1, 1
			if info.components > 1 {
				blocks = 0
			}
			for c := range info.components {
				f := seg[7+3*c]
				if f>>4 < 1 || f>>4 > 4 || f&15 < 1 || f&15 > 4 {
					return info, errImageUnreadable
				}
				if info.components > 1 {
					maxH, maxV = max(maxH, int(f>>4)), max(maxV, int(f&15))
					blocks += int(f>>4) * int(f&15)
				}
			}
			info.mcuW, info.mcuH, info.mcuBlocks = 8*maxH, 8*maxV, blocks
		case m == 0xE1 && !exif && bytes.HasPrefix(seg, []byte("Exif\x00\x00")):
			exif = true
			info.orientation = exifOrientation(seg[6:])
		}
		i += 2 + length
	}
	return info, errImageUnreadable
}

// walkPNG reads the chunk headers from IHDR to the first IDAT (at most maxHeaderSegments chunks): IHDR gives
// the size and interlacing (counted like a progressive JPEG, doubling the estimate), and an eXIf chunk the
// orientation. CRCs are left to the decoder.
func walkPNG(data []byte) (imageInfo, error) {
	info := imageInfo{format: formatPNG, orientation: 1}
	exif := false
	i := len(pngMagic)
	for n := 0; n < maxHeaderSegments; n++ {
		if i+8 > len(data) {
			return info, errImageUnreadable
		}
		length := int64(binary.BigEndian.Uint32(data[i:]))
		if length > int64(len(data)-i-12) {
			return info, errImageUnreadable
		}
		kind, body := string(data[i+4:i+8]), data[i+8:i+8+int(length)]
		switch {
		case n == 0:
			if kind != "IHDR" || length != 13 {
				return info, errImageUnreadable
			}
			w, h := binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:])
			info.width, info.height = int(min(w, 1<<30)), int(min(h, 1<<30))
			info.progressive = body[12] == 1
		case kind == "eXIf" && !exif:
			exif = true
			info.orientation = exifOrientation(body)
		case kind == "IDAT":
			return info, nil
		}
		i += 12 + int(length)
	}
	return info, errImageUnreadable
}

// exifOrientation reads tag 0x0112 (Orientation, SHORT) from the first IFD of a TIFF structure; anything
// missing, malformed or out of range is 1 (as stored).
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiff[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int64(bo.Uint32(tiff[4:]))
	if off < 8 || off+2 > int64(len(tiff)) {
		return 1
	}
	count := int64(bo.Uint16(tiff[off:]))
	for k := int64(0); k < count; k++ {
		e := off + 2 + 12*k
		if e+12 > int64(len(tiff)) {
			return 1
		}
		if bo.Uint16(tiff[e:]) != 0x0112 {
			continue
		}
		if v := int(bo.Uint16(tiff[e+8:])); bo.Uint16(tiff[e+2:]) == 3 && v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}

// encodedImage is a re-encoded upload: the full JPEG, its thumbnail JPEG and the full one's size.
type encodedImage struct {
	full, thumb    []byte
	width, height  int
	perceptualHash string // dHash of the normalized pixels; never of the original file
}

// The decoders, replaceable only so a test can make one panic (TestDecodePanicRecovered).
var (
	decodeJPEG = jpeg.Decode
	decodePNG  = png.Decode
)

// reencodeImage decodes a scanned upload that passed inspectImage and returns its re-encoded JPEGs: a photo
// fitting 1200×1200 (q82) with a thumbnail letterboxed to exactly 320×240 (q70); an avatar centre-cropped to
// 256×256 with a 64×64 thumbnail. Nothing is upscaled. The caller holds an imageDecodeWork slot.
func reencodeImage(u scannedUpload, info imageInfo, kind uploadKind) (encodedImage, error) {
	return reencode(u.Bytes(), info, kind)
}

// reencode is reencodeImage's work on the bytes. Only reencodeImage calls it, so only scanned bytes are decoded
// (TestOnlyScannedBytesAreDecoded); the fuzz test calls it directly.
func reencode(data []byte, info imageInfo, kind uploadKind) (out encodedImage, err error) {
	defer func() {
		if recover() != nil {
			out, err = encodedImage{}, errImageUnreadable
		}
	}()
	var img image.Image
	switch {
	case info.format == formatJPEG && bytes.HasPrefix(data, jpegMagic):
		img, err = decodeJPEG(bytes.NewReader(data))
	case info.format == formatPNG && bytes.HasPrefix(data, pngMagic):
		img, err = decodePNG(bytes.NewReader(data))
	default:
		return out, errImageUnreadable
	}
	if err != nil || img.Bounds() != image.Rect(0, 0, info.width, info.height) {
		return out, errImageUnreadable
	}
	var full, thumb *image.RGBA
	if kind == uploadAvatar {
		full, thumb = avatarImages(img, info.orientation)
	} else {
		full, thumb = photoImages(img, info.orientation)
	}
	out = encodedImage{full: encodeJPEG(full, photoQuality), thumb: encodeJPEG(thumb, thumbQuality), width: full.Rect.Dx(), height: full.Rect.Dy(), perceptualHash: normalizedDifferenceHash(full)}
	if len(out.full) > maxFullBytes || len(out.thumb) > maxThumbBytes {
		return encodedImage{}, errImageDetailed
	}
	return out, nil
}

// normalizedDifferenceHash is a 64-bit perceptual difference hash (dHash). It
// operates on the bounded, oriented pixels already produced by the isolated
// decoder; the application never decodes an image to verify this advisory value.
// Similar hashes are a review signal, not proof of duplication or ownership.
func normalizedDifferenceHash(full *image.RGBA) string {
	small := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.ApproxBiLinear.Scale(small, small.Rect, full, full.Rect, draw.Src, nil)
	var hash uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			hash <<= 1
			if small.GrayAt(x, y).Y > small.GrayAt(x+1, y).Y {
				hash |= 1
			}
		}
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], hash)
	return hex.EncodeToString(encoded[:])
}

func validPerceptualHash(value string) bool {
	if value == "" { // An older gateway's envelope has no perceptual metadata.
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 8 && hex.EncodeToString(decoded) == value
}

// photoImages returns the full photo (fitting 1200×1200, oriented) and its thumbnail (fitting 320×240,
// centred on a white 320×240 canvas).
func photoImages(img image.Image, orientation int) (full, thumb *image.RGBA) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if orientation >= 5 { // shown rotated a quarter turn
		w, h = h, w
	}
	fw, fh := fitWithin(w, h, photoLongEdge, photoLongEdge)
	sw, sh := fw, fh
	if orientation >= 5 {
		sw, sh = fh, fw
	}
	full = orient(resample(img, img.Bounds(), sw, sh, draw.ApproxBiLinear), orientation)
	tw, th := fitWithin(fw, fh, thumbWidth, thumbHeight)
	small := resample(full, full.Rect, tw, th, draw.CatmullRom)
	thumb = image.NewRGBA(image.Rect(0, 0, thumbWidth, thumbHeight))
	draw.Draw(thumb, thumb.Rect, image.White, image.Point{}, draw.Src)
	at := image.Pt((thumbWidth-tw)/2, (thumbHeight-th)/2)
	draw.Draw(thumb, image.Rectangle{Min: at, Max: at.Add(small.Rect.Size())}, small, image.Point{}, draw.Src)
	return full, thumb
}

// avatarImages returns the centre square, at most 256×256 and oriented, and its thumbnail (at most 64×64).
func avatarImages(img image.Image, orientation int) (full, thumb *image.RGBA) {
	b := img.Bounds()
	side := min(b.Dx(), b.Dy())
	at := b.Min.Add(image.Pt((b.Dx()-side)/2, (b.Dy()-side)/2))
	s := min(side, avatarSide)
	full = orient(resample(img, image.Rectangle{Min: at, Max: at.Add(image.Pt(side, side))}, s, s, draw.CatmullRom), orientation)
	t := min(s, avatarThumbSide)
	return full, resample(full, full.Rect, t, t, draw.CatmullRom)
}

// fitWithin scales w×h down (never up) to fit a bw×bh box, keeping the aspect ratio.
func fitWithin(w, h, bw, bh int) (int, int) {
	if w <= bw && h <= bh {
		return w, h
	}
	if w*bh >= h*bw { // width-bound
		return bw, max(1, (h*bw+w/2)/w)
	}
	return max(1, (w*bh+h/2)/h), bh
}

// resample returns sr of src at w×h (a downscale or the same size) on white. A reduction of 2× or more first
// averages k×k blocks (boxShrink), so the interpolator only ever reduces by less than 2×: the averaging stops
// aliasing, and x/image/draw's kernel scalers then need a temporary buffer of 32 bytes per pixel of w × the
// shrunk height only, instead of × the source height.
func resample(src image.Image, sr image.Rectangle, w, h int, interp draw.Interpolator) *image.RGBA {
	if k := min(sr.Dx()/w, sr.Dy()/h); k >= 2 {
		src = boxShrink(src, sr, k)
		sr = src.Bounds()
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Rect, image.White, image.Point{}, draw.Src)
	if sr.Dx() == w && sr.Dy() == h {
		draw.Draw(dst, dst.Rect, src, sr.Min, draw.Over)
	} else {
		interp.Scale(dst, dst.Rect, src, sr, draw.Over, nil)
	}
	return dst
}

// boxShrink averages the k×k blocks of sr (centred; a remainder of fewer than k pixels at the edges is
// dropped) into a premultiplied RGBA image 1/k the size.
func boxShrink(src image.Image, sr image.Rectangle, k int) *image.RGBA {
	w, h := sr.Dx()/k, sr.Dy()/k
	ox, oy := sr.Min.X+(sr.Dx()-w*k)/2, sr.Min.Y+(sr.Dy()-h*k)/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	s64, fast := src.(image.RGBA64Image)
	n := uint64(k * k)
	for y := range h {
		for x := range w {
			var r, g, b, a uint64
			for yy := oy + y*k; yy < oy+(y+1)*k; yy++ {
				for xx := ox + x*k; xx < ox+(x+1)*k; xx++ {
					var c color.RGBA64
					if fast {
						c = s64.RGBA64At(xx, yy)
					} else {
						cr, cg, cb, ca := src.At(xx, yy).RGBA()
						c = color.RGBA64{uint16(cr), uint16(cg), uint16(cb), uint16(ca)}
					}
					r, g, b, a = r+uint64(c.R), g+uint64(c.G), b+uint64(c.B), a+uint64(c.A)
				}
			}
			p := dst.PixOffset(x, y)
			dst.Pix[p], dst.Pix[p+1], dst.Pix[p+2], dst.Pix[p+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(b/n>>8), uint8(a/n>>8)
		}
	}
	return dst
}

// orient turns an image stored with EXIF orientation o into how it is meant to be seen: 2 mirrored, 3 turned
// 180°, 4 flipped, 5 transposed, 6 turned 90° clockwise, 7 transversed, 8 turned 90° anticlockwise.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	W, H := w, h
	if o >= 5 {
		W, H = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, W, H))
	for Y := range H {
		for X := range W {
			var x, y int
			switch o {
			case 2:
				x, y = w-1-X, Y
			case 3:
				x, y = w-1-X, h-1-Y
			case 4:
				x, y = X, h-1-Y
			case 5:
				x, y = Y, X
			case 6:
				x, y = Y, h-1-X
			case 7:
				x, y = w-1-Y, h-1-X
			case 8:
				x, y = w-1-Y, X
			}
			d, s := dst.PixOffset(X, Y), src.PixOffset(x, y)
			copy(dst.Pix[d:d+4], src.Pix[s:s+4])
		}
	}
	return dst
}

func encodeJPEG(img image.Image, quality int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
		panic(err) // writes to a bytes.Buffer do not fail; recovered by reencodeImage
	}
	return b.Bytes()
}
