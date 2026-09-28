package market

import (
	"html"
	"html/template"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// paymentURI is the wallet payment request for the amount still due (atomic units) to a deposit address: BIP21
// for Bitcoin ("bitcoin:<address>?amount=<BTC>") and the Monero URI scheme ("monero:<address>?tx_amount=<XMR>").
// The amount is the same decimal the order page shows as remaining, so a wallet that scans it pre-fills exactly
// what is due. Nothing else (no label or message) is included: it would be stored in the payer's wallet history.
func paymentURI(currency, address string, due int64) string {
	if currency == "XMR" {
		return "monero:" + address + "?tx_amount=" + amount(due, currencyDecimals(currency))
	}
	return "bitcoin:" + address + "?amount=" + amount(due, currencyDecimals(currency))
}

// qrQuiet is the quiet zone around the symbol, in modules (the QR specification's minimum).
const qrQuiet = 4

// qrSVG renders text as an inline SVG QR code: error correction level M, dark modules on a white background with
// a quiet zone, one path of horizontal runs. It is inline markup rather than an image, so the page's CSP
// (img-src 'self', no inline styles) and no-JavaScript rule hold; label names it for screen readers.
func qrSVG(text, label string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	n := code.Size + 2*qrQuiet
	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; {
			if !code.Black(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < code.Size && code.Black(x+run, y) {
				run++
			}
			path.WriteString("M" + strconv.Itoa(x+qrQuiet) + " " + strconv.Itoa(y+qrQuiet) + "h" + strconv.Itoa(run) + "v1h-" + strconv.Itoa(run) + "z")
			x += run
		}
	}
	size := strconv.Itoa(n)
	return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + size + ` ` + size + `" width="240" height="240" role="img" aria-label="` +
		html.EscapeString(label) + `" shape-rendering="crispEdges"><rect width="` + size + `" height="` + size + `" fill="#fff"/><path fill="#000" d="` +
		path.String() + `"/></svg>`), nil
}
