package btc

import "strings"

// The BIP 380 output descriptor checksum (Bitcoin Core's "#…" suffix).

const (
	descInputCharset    = "0123456789()[],'/*abcdefgh@:$%{}IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
	descChecksumCharset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

var descGenerator = [5]uint64{0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d, 0x3706b1677a, 0x644d626ffd}

func descPolymod(c uint64, v int) uint64 {
	top := c >> 35
	c = (c&0x7ffffffff)<<5 ^ uint64(v)
	for i, g := range descGenerator {
		if top>>i&1 != 0 {
			c ^= g
		}
	}
	return c
}

// DescriptorChecksum is the 8-character BIP 380 checksum of desc; false for a character outside the descriptor
// alphabet.
func DescriptorChecksum(desc string) (string, bool) {
	c := uint64(1)
	cls, clsCount := 0, 0
	for i := 0; i < len(desc); i++ {
		pos := strings.IndexByte(descInputCharset, desc[i])
		if pos < 0 {
			return "", false
		}
		c = descPolymod(c, pos&31)
		cls = cls*3 + pos>>5
		if clsCount++; clsCount == 3 {
			c = descPolymod(c, cls)
			cls, clsCount = 0, 0
		}
	}
	if clsCount > 0 {
		c = descPolymod(c, cls)
	}
	for range 8 {
		c = descPolymod(c, 0)
	}
	c ^= 1
	out := make([]byte, 8)
	for i := range out {
		out[i] = descChecksumCharset[c>>(5*(7-i))&31]
	}
	return string(out), true
}

// WithChecksum is desc followed by "#" and its checksum; desc must be in the descriptor alphabet.
func WithChecksum(desc string) string {
	sum, _ := DescriptorChecksum(desc)
	return desc + "#" + sum
}
