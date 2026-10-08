package market

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// A-84: pooled deposit identifiers let a vendor trace the buyer's wallet activity on the public chain.
// The stored ledger, order history, and historical notifications stay unchanged; vendor-facing text is redacted
// when rendered. Escrow has different verification needs and is deliberately excluded from these queries.
const (
	pooledAddressHidden = "(deposit address shown only to the buyer)"
	pooledTxidHidden    = "(transaction ID shown only to the buyer)"
)

type pooledQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// hidePooledOrderIdentifiers also works inside flagOrder's transaction, including deposits written in that pass.
func hidePooledOrderIdentifiers(ctx context.Context, q pooledQueryer, orderID string, texts ...*string) error {
	rows, err := q.QueryContext(ctx, `SELECT pa.address,false FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id
		WHERE pa.order_id=$1 AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id) AND pa.address<>''
		UNION ALL SELECT pm.txid,true FROM payments pm JOIN orders o ON o.id=pm.order_id
		WHERE pm.order_id=$1 AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id) AND pm.txid<>''`, orderID)
	if err != nil {
		return err
	}
	return replacePooledIdentifiers(rows, texts)
}

// A notification written before A-84 may contain a pooled identifier. Derive candidates from only the 100 bodies
// actually rendered, then use indexed exact address/txid and 20-character txid-prefix lookups. No age or ledger
// limit is applied: an old identifier in a current notification is still hidden. Payment addresses on both supported
// networks and the watcher's txids are ASCII word-like strings; punctuation (including URI separators) splits them.
var pooledNoticeToken = regexp.MustCompile(`[A-Za-z0-9_-]{4,128}`)

func hidePooledVendorNotifications(ctx context.Context, q pooledQueryer, vendorID string, notices []Notification) error {
	if len(notices) == 0 {
		return nil
	}
	texts := make([]*string, 0, len(notices))
	words, prefixes := map[string]bool{}, map[string]bool{}
	for i := range notices {
		texts = append(texts, &notices[i].Body)
		body := notices[i].Body
		for _, span := range pooledNoticeToken.FindAllStringIndex(body, -1) {
			word := body[span[0]:span[1]]
			words[word] = true
			if len(word) == 20 && strings.HasPrefix(body[span[1]:], "…") {
				prefixes[word] = true
			}
		}
	}
	if len(words) == 0 {
		return nil
	}
	wordList := make([]string, 0, len(words))
	for word := range words {
		wordList = append(wordList, word)
	}
	prefixList := make([]string, 0, len(prefixes))
	for prefix := range prefixes {
		prefixList = append(prefixList, prefix)
	}
	rows, err := q.QueryContext(ctx, `SELECT pa.address,false FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id
		JOIN products p ON p.id=o.product_id WHERE pa.address=ANY($2) AND p.vendor_id=$1
		AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)
		UNION ALL SELECT pm.txid,true FROM payments pm JOIN orders o ON o.id=pm.order_id
		JOIN products p ON p.id=o.product_id WHERE pm.currency='BTC' AND pm.txid=ANY($2) AND p.vendor_id=$1
		AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)
		UNION ALL SELECT pm.txid,true FROM payments pm JOIN orders o ON o.id=pm.order_id
		JOIN products p ON p.id=o.product_id WHERE pm.currency='XMR' AND pm.txid=ANY($2) AND p.vendor_id=$1
		AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)
		UNION ALL SELECT pm.txid,true FROM payments pm JOIN orders o ON o.id=pm.order_id
		JOIN products p ON p.id=o.product_id WHERE left(pm.txid,20)=ANY($3) AND p.vendor_id=$1
		AND NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id)`, vendorID, wordList, prefixList)
	if err != nil {
		return err
	}
	return replacePooledIdentifiers(rows, texts)
}

func replacePooledIdentifiers(rows *sql.Rows, texts []*string) error {
	defer rows.Close()
	for rows.Next() {
		var value string
		var txid bool
		if err := rows.Scan(&value, &txid); err != nil {
			return err
		}
		hidden := pooledAddressHidden
		if txid {
			hidden = pooledTxidHidden
		}
		for _, text := range texts {
			*text = strings.ReplaceAll(*text, value, hidden)
			if txid {
				*text = strings.ReplaceAll(*text, truncate(value, 20), hidden)
			}
		}
	}
	return rows.Err()
}
