package market

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"strings"
)

// Image CAPTCHA: six characters from captchaAlphabet drawn with a 5x7 bitmap font, then scaled,
// jittered, sheared, waved and crossed with noise. Text and distortion derive from
// HMAC(a.key, "captcha:"+id), so the server stores only a hash of the answer and a given id always
// renders the same image (reloading cannot be used to average the noise away).

const (
	captchaLen    = 6
	captchaWidth  = 200
	captchaHeight = 70
)

func (a *App) captchaSeed(id string) [32]byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte("captcha:" + id))
	var seed [32]byte
	copy(seed[:], m.Sum(nil))
	return seed
}

func captchaText(seed [32]byte) string {
	b := make([]byte, captchaLen)
	for i := range b {
		b[i] = captchaAlphabet[int(binary.BigEndian.Uint16(seed[2*i:]))%len(captchaAlphabet)]
	}
	return string(b)
}

// captchaAnswer is the expected text for id (tests and the image renderer use it).
func (a *App) captchaAnswer(id string) string { return captchaText(a.captchaSeed(id)) }

func normalizeCaptcha(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

func captchaAnswerHash(id, answer string) string {
	return digest("captcha:" + id + ":" + normalizeCaptcha(answer))
}

var captchaPalette = color.Palette{
	color.RGBA{0x18, 0x18, 0x16, 0xff}, // background
	color.RGBA{0xf0, 0xed, 0xe6, 0xff}, // ink
	color.RGBA{0xff, 0xb5, 0x2e, 0xff},
	color.RGBA{0xc9, 0xd6, 0xe8, 0xff},
	color.RGBA{0x4a, 0x48, 0x42, 0xff}, // speckle
	color.RGBA{0x6b, 0x67, 0x5e, 0xff},
}

func (a *App) captchaImage(id string) *image.Paletted {
	seed := a.captchaSeed(id)
	return renderCaptcha(captchaText(seed), seed)
}

func renderCaptcha(text string, seed [32]byte) *image.Paletted {
	rng := rand.New(rand.NewChaCha8(seed))
	img := image.NewPaletted(image.Rect(0, 0, captchaWidth, captchaHeight), captchaPalette)
	set := func(x, y int, c uint8) {
		if x >= 0 && y >= 0 && x < captchaWidth && y < captchaHeight {
			img.SetColorIndex(x, y, c)
		}
	}
	for range 400 {
		set(rng.IntN(captchaWidth), rng.IntN(captchaHeight), uint8(4+rng.IntN(2)))
	}
	amp, period, phase := 2+rng.Float64()*2, 45+rng.Float64()*30, rng.Float64()*2*math.Pi
	for i, r := range text {
		g := captchaFont[r]
		sc := 4 + rng.IntN(2)
		ox := 12 + i*30 + rng.IntN(7) - 3
		oy := 17 + rng.IntN(11) - 5
		shear := (rng.Float64() - 0.5) * 0.6
		ink := uint8(1 + rng.IntN(3))
		mid := float64(oy) + 3.5*float64(sc)
		for row := range 7 {
			for col := range 5 {
				if g[row][col] != '#' {
					continue
				}
				for dy := range sc {
					for dx := range sc {
						y := oy + row*sc + dy
						x := ox + col*sc + dx + int(shear*(float64(y)-mid))
						set(x, y+int(amp*math.Sin(2*math.Pi*float64(x)/period+phase)), ink)
					}
				}
			}
		}
	}
	for i := range 5 {
		x0, y0 := rng.IntN(captchaWidth/4), rng.IntN(captchaHeight)
		x1, y1 := captchaWidth-1-rng.IntN(captchaWidth/4), rng.IntN(captchaHeight)
		c, thick := uint8(1+rng.IntN(3)), 1+i%2
		captchaLine(x0, y0, x1, y1, func(x, y int) {
			for t := range thick {
				set(x, y+t, c)
			}
		})
	}
	return img
}

// captchaLine plots a Bresenham line.
func captchaLine(x0, y0, x1, y1 int, plot func(x, y int)) {
	dx, dy := captchaAbs(x1-x0), -captchaAbs(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	e := dx + dy
	for {
		plot(x0, y0)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

func captchaAbs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
