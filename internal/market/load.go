package market

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A-181 (with A-16 catalog paging, A-60 PA-8): the catalog on / is searched with a capped, literal term and
// served 25 listings a page, each card showing an excerpt; the product page keeps the full description.
const (
	searchMaxBytes  = 64  // q beyond this is ignored (cut on a rune boundary)
	filterMaxBytes  = 80  // category and region filters (a listing region is at most 80 bytes)
	catalogPageSize = 25  // listings per catalog page
	excerptRunes    = 300 // characters of description on a listing card, including the trailing "…"
)

// cleanParam drops NUL bytes and invalid UTF-8 (PostgreSQL refuses both, which would be a 500), trims
// surrounding space and cuts s to at most limit bytes without splitting a character.
func cleanParam(s string, limit int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), ""))
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func searchTerm(s string) string  { return cleanParam(s, searchMaxBytes) }
func filterValue(s string) string { return cleanParam(s, filterMaxBytes) }

// normaliseCatalogFilters applies the caps to the search and filter values every page echoes in its header.
func normaliseCatalogFilters(d *PageData) {
	d.Query, d.Category, d.Region = searchTerm(d.Query), filterValue(d.Category), filterValue(d.Region)
}

// likePattern matches term literally anywhere in a column compared with ILIKE ... ESCAPE '\': the
// wildcards % and _ and the escape character itself are escaped.
func likePattern(term string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(term) + "%"
}

// Excerpt is the description shown on a listing card: at most excerptRunes characters, cut on a character
// boundary and marked with "…" when shortened. Listing-grid pages load only the first excerptRunes+1
// characters of each description (load), which is enough to tell whether it was cut.
func (p Product) Excerpt() string {
	if utf8.RuneCountInString(p.Description) <= excerptRunes {
		return p.Description
	}
	n := 0
	for i := range p.Description {
		if n == excerptRunes-1 {
			return strings.TrimRight(p.Description[:i], " \t\r\n") + "…"
		}
		n++
	}
	return p.Description
}

// CatalogPaging is the catalog's position: Page of Pages (1-based), listings First..Last of Total, and the
// previous/next page links ("" at either end) carrying the same search, filters and display currency.
type CatalogPaging struct {
	Page, Pages, Total, First, Last int
	PrevURL, NextURL                string
}

// Offset is the number of listings before this page.
func (p CatalogPaging) Offset() int { return (p.Page - 1) * catalogPageSize }

// PrevPage and NextPage number the pages PrevURL and NextURL lead to (for the links' accessible names).
func (p CatalogPaging) PrevPage() int { return p.Page - 1 }
func (p CatalogPaging) NextPage() int { return p.Page + 1 }

// catalogPaging reads the requested page: anything but a positive decimal number is page 1, and a page past
// the end (including one too large to parse) is the last page, so every link and typed URL lands somewhere.
func catalogPaging(d *PageData, total int, raw string) CatalogPaging {
	p := CatalogPaging{Page: 1, Total: total, Pages: max(1, (total+catalogPageSize-1)/catalogPageSize)}
	if n, err := strconv.Atoi(raw); err == nil && n > 1 {
		p.Page = min(n, p.Pages)
	} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
		p.Page = p.Pages
	}
	if total > 0 {
		p.First, p.Last = p.Offset()+1, min(p.Offset()+catalogPageSize, total)
	}
	link := func(page int) string {
		v := url.Values{}
		for k, x := range map[string]string{"q": d.Query, "category": d.Category, "region": d.Region} {
			if x != "" {
				v.Set(k, x)
			}
		}
		if d.Currency == "XMR" {
			v.Set("currency", "XMR")
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		if len(v) == 0 {
			return "/"
		}
		return "/?" + v.Encode()
	}
	if p.Page > 1 {
		p.PrevURL = link(p.Page - 1)
	}
	if p.Page < p.Pages {
		p.NextURL = link(p.Page + 1)
	}
	return p
}

// pageSlice is catalogPaging for an in-memory list (the read-only preview's sample listings).
func pageSlice(d *PageData, all []Product, raw string) []Product {
	p := catalogPaging(d, len(all), raw)
	d.Paging = &p
	if len(all) == 0 {
		return all
	}
	return all[p.First-1 : p.Last]
}

// load fills generic page data, then runs registered loaders (routes.go).
func (a *App) load(r *http.Request, d *PageData) error {
	ctx := r.Context()
	rows, err := a.db.QueryContext(ctx, "SELECT key,value FROM settings")
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		d.Settings[k] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	normaliseCatalogFilters(d)
	if d.Page == "catalog" || d.Page == "product" || d.Page == "vendor" || d.Page == "checkout" || d.Page == "vendor-dashboard" {
		// An administrator's takedown reason (A-163) is read only for the owner's vendor desk.
		takedown := "''"
		if d.Page == "vendor-dashboard" {
			takedown = "coalesce(p.admin_archive_reason,'')"
		}
		// Listing grids (catalog, vendor, vendor desk) render Product.Excerpt, so they load only its prefix;
		// product and checkout pages load and show the full description.
		description := "p.description"
		if d.Page == "catalog" || d.Page == "vendor" || d.Page == "vendor-dashboard" {
			description = "left(p.description," + strconv.Itoa(excerptRunes+1) + ")"
		}
		query := `SELECT p.id,p.title,` + description + `,p.category,p.region,p.kind,u.handle,p.vendor_id,p.btc,p.xmr,p.stock,p.archived,` + takedown + ` FROM products p JOIN users u ON u.id=p.vendor_id WHERE true`
		args := []any{}
		limit := " LIMIT 100"
		switch d.Page {
		case "product", "checkout":
			query += " AND p.id=$1"
			args = append(args, r.URL.Query().Get("id"))
		case "vendor":
			query += " AND p.vendor_id=$1"
			args = append(args, r.URL.Query().Get("id"))
		case "vendor-dashboard":
			query += " AND p.vendor_id=$1"
			args = append(args, d.User.ID)
		case "catalog":
			// The term is matched literally (likePattern); the count and the page share one filter. E'\\' is one
			// backslash whatever standard_conforming_strings an external database sets.
			filter := ` AND NOT p.archived AND ($1='' OR p.title ILIKE $4 ESCAPE E'\\' OR p.description ILIKE $4 ESCAPE E'\\') AND ($2='' OR p.category=$2) AND ($3='' OR p.region=$3 OR p.region='Worldwide')`
			args = append(args, d.Query, d.Category, d.Region, likePattern(d.Query))
			var total int
			if err = a.db.QueryRowContext(ctx, `SELECT count(*) FROM products p WHERE true`+filter, args...).Scan(&total); err != nil {
				return err
			}
			paging := catalogPaging(d, total, r.URL.Query().Get("page"))
			d.Paging = &paging
			query += filter
			limit = " LIMIT " + strconv.Itoa(catalogPageSize) + " OFFSET " + strconv.Itoa(paging.Offset())
		}
		if d.Page != "vendor-dashboard" && d.Page != "catalog" {
			query += " AND NOT p.archived"
		}
		// p.id breaks ties between listings created in the same transaction, so pages never overlap.
		query += " ORDER BY p.created DESC, p.id DESC" + limit
		rows, err = a.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p Product
			var btc, xmr int64
			if err = rows.Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Region, &p.Kind, &p.Vendor, &p.VendorID, &btc, &xmr, &p.Stock, &p.Archived, &p.TakedownReason); err != nil {
				rows.Close()
				return err
			}
			p.PriceBTC = amount(btc, 8)
			p.PriceXMR = amount(xmr, 12)
			d.Products = append(d.Products, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if d.Page == "product" || d.Page == "checkout" {
			if len(d.Products) == 0 {
				return sql.ErrNoRows
			}
			d.Product = &d.Products[0]
		}
		if d.Page == "vendor" {
			var u User
			err = a.db.QueryRowContext(ctx, "SELECT id,handle,role,pgp,xmpp FROM users WHERE id=$1 AND role IN ('vendor','admin')", r.URL.Query().Get("id")).Scan(&u.ID, &u.Handle, &u.Role, &u.PGP, &u.XMPP)
			if err != nil {
				return err
			}
			d.Users = []User{u}
		}
	}
	if d.User != nil {
		switch d.Page {
		case "orders", "order", "admin", "disputes":
			query := orderQuery + " WHERE (o.buyer_id=$1 OR p.vendor_id=$1)"
			args := []any{d.User.ID}
			if d.Page == "admin" {
				query = orderQuery + " WHERE true"
				args = nil
			}
			if d.Page == "order" {
				// Moderators and administrators may also open disputed or resolved orders, and orders with a payment
				// flagged for review, read-only (orders_load.go).
				query = orderQuery + " WHERE (o.buyer_id=$1 OR p.vendor_id=$1 OR ($3 IN ('moderator','admin') AND (o.state IN ('disputed','resolved') OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.flagged)))) AND o.id=$2"
				args = append(args, r.URL.Query().Get("id"), d.User.Role)
			}
			if d.Page == "disputes" {
				// Only disputable orders, and every one of them: an old shipped order must stay selectable.
				query += " AND o.state IN ('paid','shipped','delivered') ORDER BY o.created DESC"
			} else {
				query += " ORDER BY o.created DESC LIMIT 100"
			}
			rows, err = a.db.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				o, err := scanOrder(rows)
				if err != nil {
					rows.Close()
					return err
				}
				d.Orders = append(d.Orders, o)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if d.Page == "order" {
				if len(d.Orders) == 0 {
					return sql.ErrNoRows
				}
				d.Order = &d.Orders[0]
			}
		}
		if d.Page == "messages" {
			rows, err = a.db.QueryContext(ctx, `SELECT m.id,s.handle,t.handle,m.body,to_char(m.created,'YYYY-MM-DD HH24:MI "UTC"') FROM messages m JOIN users s ON s.id=m.sender_id JOIN users t ON t.id=m.recipient_id WHERE m.sender_id=$1 OR m.recipient_id=$1 ORDER BY m.created DESC LIMIT 100`, d.User.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var m Message
				if err = rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Body, &m.Created); err != nil {
					rows.Close()
					return err
				}
				d.Messages = append(d.Messages, m)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		if d.Page == "notifications" {
			rows, err = a.db.QueryContext(ctx, `SELECT id,body,to_char(created,'YYYY-MM-DD HH24:MI "UTC"'),is_read FROM notifications WHERE user_id=$1 ORDER BY created DESC LIMIT 100`, d.User.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var n Notification
				if err = rows.Scan(&n.ID, &n.Body, &n.Created, &n.Read); err != nil {
					rows.Close()
					return err
				}
				d.Notifications = append(d.Notifications, n)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		if d.Page == "admin" {
			rows, err = a.db.QueryContext(ctx, "SELECT id,handle,role,pgp,xmpp FROM users ORDER BY created DESC LIMIT 100")
			if err != nil {
				return err
			}
			for rows.Next() {
				var u User
				if err = rows.Scan(&u.ID, &u.Handle, &u.Role, &u.PGP, &u.XMPP); err != nil {
					rows.Close()
					return err
				}
				d.Users = append(d.Users, u)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		if d.Page == "account" || d.Page == "admin" {
			// The admin audit trail lists every user's events; the account page shows only the viewer's recent ones.
			all, limit := d.Page == "admin" && d.User.Role == "admin", 100
			if d.Page == "account" {
				limit = 15
			}
			rows, err = a.db.QueryContext(ctx, `SELECT COALESCE(u.handle,''),e.action,to_char(e.created,'YYYY-MM-DD HH24:MI "UTC"') FROM audit_events e LEFT JOIN users u ON u.id=e.user_id
				WHERE e.user_id=$1 OR $2 ORDER BY e.created DESC,e.id DESC LIMIT $3`, d.User.ID, all, limit)
			if err != nil {
				return err
			}
			for rows.Next() {
				var e Event
				if err = rows.Scan(&e.Handle, &e.Action, &e.Created); err != nil {
					rows.Close()
					return err
				}
				d.Events = append(d.Events, e)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
	}
	return runLoaders(ctx, a, r, d)
}

func previewData(page string) PageData {
	p := []Product{
		{ID: "encrypted-drive", Title: "Encrypted USB Drive — 256GB", Description: "Hardware-encrypted storage with a tamper-evident enclosure. A practical place for your offline backups and private documents.", Category: "Hardware", Region: "Europe", Kind: "physical", Vendor: "ghost_circuit", VendorID: "ghost", PriceBTC: "0.00412", PriceXMR: "0.271", Stock: 7},
		{ID: "security-research", Title: "Security Research — Q3 Report", Description: "Defensive analysis of patched vulnerabilities, mitigation recommendations, and a clear guide to keeping your systems up to date.", Category: "Digital", Region: "Worldwide", Kind: "digital", Vendor: "null_ptr", VendorID: "null", PriceBTC: "0.00089", PriceXMR: "0.058", Stock: 99},
		{ID: "privacy-course", Title: "Practical Privacy — Field Guide", Description: "An independent guide to device hygiene, threat modeling, and safer everyday communication.", Category: "Digital", Region: "Worldwide", Kind: "digital", Vendor: "cipher_monk", VendorID: "cipher", PriceBTC: "0.00031", PriceXMR: "0.020", Stock: 99},
		{ID: "faraday-bag", Title: "Faraday Laptop Bag — 15 inch", Description: "A signal-shielding sleeve designed for storing your laptop and small devices. Check manufacturer specifications before purchase.", Category: "Hardware", Region: "Worldwide", Kind: "physical", Vendor: "iron_cage", VendorID: "iron", PriceBTC: "0.00156", PriceXMR: "0.103", Stock: 23},
		{ID: "node-guide", Title: "Self-hosting — Node Setup Guide", Description: "Documentation for maintaining your own infrastructure, with monitoring and backup planning.", Category: "Digital", Region: "Worldwide", Kind: "digital", Vendor: "null_ptr", VendorID: "null", PriceBTC: "0.00042", PriceXMR: "0.028", Stock: 99},
		{ID: "consulting", Title: "Personal Security Consultation", Description: "A one-hour review of your everyday privacy requirements and a practical plan for reducing unnecessary data exposure.", Category: "Services", Region: "Worldwide", Kind: "digital", Vendor: "cipher_monk", VendorID: "cipher", PriceBTC: "0.0012", PriceXMR: "0.079", Stock: 4},
	}
	u := &User{ID: "preview", Handle: "preview_user", Role: "buyer"}
	if page == "admin" {
		u.Role = "admin"
	}
	if page == "moderator" {
		u.Role = "moderator"
	}
	if page == "vendor-dashboard" {
		u.Role = "vendor"
	}
	o := Order{ID: previewDraftID, ProductID: p[0].ID, Title: p[0].Title, Buyer: u.Handle, Vendor: p[0].Vendor, Currency: "BTC", Amount: p[0].PriceBTC, State: stateDraft, Status: stateLabel(stateDraft), Kind: p[0].Kind, BuyerID: u.ID, VendorID: p[0].VendorID, Created: "Sample order", Updated: "Sample order"}
	return PageData{Page: page, Title: strings.ReplaceAll(strings.Title(page), "-", " "), Preview: true, User: u, Products: p, Product: &p[0], Orders: []Order{o}, Order: &o, Currency: "BTC", Settings: map[string]string{"site_name": "OPSMKT"}, Users: []User{{ID: "ghost", Handle: "ghost_circuit", Role: "vendor"}}, Events: []Event{{Handle: u.Handle, Action: "Preview only — no real activity", Created: "—"}}}
}
