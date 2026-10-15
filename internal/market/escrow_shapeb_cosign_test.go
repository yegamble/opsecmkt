package market

// signRequests is the sign requests the fake signer read.
func (f *fakeSigner) signRequests() []map[string]any {
	var out []map[string]any
	for _, r := range f.reqs() {
		if r["op"] == "sign" {
			out = append(out, r)
		}
	}
	return out
}
