package market

import (
	"encoding/hex"

	"strconv"
)

// perOrderSetupWant is what the setup page shows reader of order: its own origin with the per-order index, the
// derivation path, the cosigner Vpubs (the other party's, the market's) and the Sparrow descriptor.
func (x *escrowEnv) perOrderSetupWant(order, reader string, signer bip32Key) (ownPath, deriv string, cosigners []string, sparrow string) {
	x.t.Helper()
	rows := x.escrowRows(order)
	exprs := map[string]string{"buyer": x.buyerKey, "vendor": x.vendorKey}
	desc := map[string]string{}
	for _, party := range []string{"buyer", "vendor", "market"} {
		steps, _ := parseOriginPath(rows[party].suffix)
		i := strconv.FormatUint(uint64(steps[0]), 10)
		account := signer
		if party != "market" {
			account = accountKey(x.t, exprs[party])
		}
		child, _ := perOrderChild(x.t, account, steps[0])
		fp := child.Fingerprint()
		switch {
		case party == reader:
			k := mustParseKey(x.t, exprs[party])
			ownPath, deriv = "["+k.Fingerprint+k.PathText+"/"+i+"]", "m"+k.PathText+"/"+i
			desc[party] = ownPath + child.Serialize(versionTpub) + "/<0;1>/*"
		default:
			cosigners = append(cosigners, child.Rooted(versionVpub))
			desc[party] = "[" + hex.EncodeToString(fp[:]) + "]" + child.Rooted(versionTpub) + "/<0;1>/*"
		}
	}
	return ownPath, deriv, cosigners, withChecksum("wsh(sortedmulti(2," + desc["buyer"] + "," + desc["vendor"] + "," + desc["market"] + "))")
}
