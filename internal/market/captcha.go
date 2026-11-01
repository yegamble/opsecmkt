package market

import (
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Image CAPTCHA: six characters from captchaAlphabet drawn with a 5x7 bitmap font, then scaled,
// jittered, sheared, waved and crossed with noise. Text and distortion derive from
// HMAC(a.key, "captcha:"+id), so a given id always renders the same image (reloading cannot be used to
// average the noise away) and the server stores nothing to check an answer.
//
// Challenges are stateless (A-176): the id is expiry ‖ nonce ‖ MAC, where the MAC is
// HMAC(a.captchaIDKey, expiry ‖ nonce ‖ session hash) truncated to 128 bits, so issuing one writes nothing
// and only the issuing browser session can use it until it expires. Single use is enforced when an answer is
// posted, by inserting the id into captcha_spent (captcha_actions.go).
//
// Written question (A-725): the same ids carry a question in plain words instead, for registration in the default
// mode. It derives from HMAC(a.key, "captcha-text:"+id) and is printed in the form's label, so a screen reader can read
// it. It stops only a bot that does not read the page: a script written for this market answers every question, and
// even a constant guess that never reads the page succeeds about one time in twenty.

const (
	captchaLen    = 6
	captchaWidth  = 200
	captchaHeight = 70
	captchaTTL    = 10 * time.Minute
)

// Challenge id layout (32 bytes, 64 lowercase hex characters, the same shape as the pre-A-176 random ids):
// 4-byte big-endian expiry in Unix seconds, 12-byte random nonce, 16-byte truncated HMAC-SHA256.
const (
	captchaExpiryLen = 4
	captchaNonceLen  = 12
	captchaMACLen    = 16
	captchaIDLen     = captchaExpiryLen + captchaNonceLen + captchaMACLen
)

// captchaMAC authenticates expiry ‖ nonce for the session whose token hashes to sessionHash.
func (a *App) captchaMAC(head []byte, sessionHash string) []byte {
	m := hmac.New(sha256.New, a.captchaIDKey)
	m.Write([]byte("captcha-id:"))
	m.Write(head)
	m.Write([]byte(sessionHash))
	return m.Sum(nil)[:captchaMACLen]
}

// newCaptchaID mints a challenge for the session whose token hashes to sessionHash, valid until expires. It
// returns "" when no challenge key is configured (never in a started App).
func (a *App) newCaptchaID(sessionHash string, expires time.Time) string {
	if len(a.captchaIDKey) == 0 {
		return ""
	}
	b := make([]byte, captchaIDLen)
	binary.BigEndian.PutUint32(b, uint32(expires.Unix()))
	if _, err := crand.Read(b[captchaExpiryLen : captchaExpiryLen+captchaNonceLen]); err != nil {
		panic(err)
	}
	head := b[:captchaExpiryLen+captchaNonceLen]
	copy(b[len(head):], a.captchaMAC(head, sessionHash))
	return hex.EncodeToString(b)
}

// captchaIDValid reports whether id was minted by this server for one of sessionHashes and has not expired
// at now, and returns its expiry. It reads nothing but id: whether it was already used is checked at POST.
func (a *App) captchaIDValid(id string, now time.Time, sessionHashes ...string) (time.Time, bool) {
	if len(a.captchaIDKey) == 0 || !captchaID.MatchString(id) {
		return time.Time{}, false
	}
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != captchaIDLen {
		return time.Time{}, false
	}
	head, mac := b[:captchaExpiryLen+captchaNonceLen], b[captchaExpiryLen+captchaNonceLen:]
	expires := time.Unix(int64(binary.BigEndian.Uint32(head)), 0)
	ok := false
	for _, h := range sessionHashes {
		// Every candidate is checked, so the time taken does not say which session matched.
		if subtle.ConstantTimeCompare(mac, a.captchaMAC(head, h)) == 1 {
			ok = true
		}
	}
	if !ok || !now.Before(expires) {
		return time.Time{}, false
	}
	return expires, true
}

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

// captchaMatches compares answer with id's text in constant time (case and spaces ignored).
func (a *App) captchaMatches(id, answer string) bool {
	return subtle.ConstantTimeCompare([]byte(normalizeCaptcha(answer)), []byte(a.captchaAnswer(id))) == 1
}

// captchaTextMaxLen bounds an answer to a written question; a longer one is wrong without being compared.
const captchaTextMaxLen = 16

// textChallenge is a written question and the normalized answers it accepts.
type textChallenge struct {
	Question string
	Answers  []string
}

// captchaNumberWords spell every number a sum can be, so "twelve" and "12" both answer.
var captchaNumberWords = [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
	"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen"}

var captchaDays = [...]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// captchaWordKinds are the kinds of word a question picks one of. No word is in two kinds or could fairly be read as
// another kind, so the other kinds' words are always wrong.
var captchaWordKinds = map[string][]string{
	"a colour":  {"red", "green", "blue", "yellow", "white", "purple", "brown", "black"},
	"an animal": {"horse", "cow", "sheep", "fish", "lion", "mouse", "rabbit", "duck"},
	"a fruit":   {"apple", "banana", "pear", "mango", "grape", "melon", "pineapple", "coconut"},
	"a tool":    {"hammer", "saw", "spade", "drill", "wrench", "chisel", "rake", "pliers"},
}

// captchaWordKindNames fixes the order of captchaWordKinds, which seed bytes index.
var captchaWordKindNames = [...]string{"a colour", "an animal", "a fruit", "a tool"}

func (a *App) captchaTextSeed(id string) [32]byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte("captcha-text:" + id))
	var seed [32]byte
	copy(seed[:], m.Sum(nil))
	return seed
}

// captchaTextChallenge is the written question for id.
func (a *App) captchaTextChallenge(id string) textChallenge {
	seed := a.captchaTextSeed(id)
	switch seed[0] % 3 {
	case 0:
		x, y := int(seed[1]%9)+1, int(seed[2]%9)+1 // both 1-9: a sum is at most 18, a difference at least 0
		op, result := "plus", x+y
		if seed[3]%2 == 0 {
			if x < y {
				x, y = y, x
			}
			op, result = "minus", x-y
		}
		return textChallenge{
			Question: "What is " + captchaNumberWords[x] + " " + op + " " + captchaNumberWords[y] + "?",
			Answers:  []string{strconv.Itoa(result), captchaNumberWords[result]},
		}
	case 1:
		i := int(seed[1]) % len(captchaDays)
		return textChallenge{
			Question: "What is the day after " + captchaDays[i] + "?",
			Answers:  []string{strings.ToLower(captchaDays[(i+1)%len(captchaDays)])},
		}
	}
	right := int(seed[1]) % len(captchaWordKindNames)
	// Four words, one from each kind, in an order the seed turns, so the right word can be at any position.
	words := make([]string, len(captchaWordKindNames))
	for i, name := range captchaWordKindNames {
		list := captchaWordKinds[name]
		words[(i+int(seed[2]))%len(words)] = list[int(seed[3+i])%len(list)]
	}
	return textChallenge{
		Question: "Which of these words is " + captchaWordKindNames[right] + ": " + strings.Join(words, ", ") + "?",
		Answers:  []string{words[(right+int(seed[2]))%len(words)]},
	}
}

// accepts compares answer with each accepted answer (case, surrounding space and a closing full stop ignored; an
// over-long one is wrong).
func (c textChallenge) accepts(answer string) bool {
	answer = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(answer), ".")))
	if len(answer) > captchaTextMaxLen {
		return false
	}
	ok := false
	for _, want := range c.Answers {
		if subtle.ConstantTimeCompare([]byte(answer), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
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
