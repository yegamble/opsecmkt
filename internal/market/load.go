package market

import (
	"database/sql"
	"net/http"
	"strings"
)

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
	if d.Page == "catalog" || d.Page == "product" || d.Page == "vendor" || d.Page == "checkout" || d.Page == "vendor-dashboard" {
		query := `SELECT p.id,p.title,p.description,p.category,p.region,p.kind,u.handle,p.vendor_id,p.btc,p.xmr,p.stock,p.archived FROM products p JOIN users u ON u.id=p.vendor_id WHERE true`
		args := []any{}
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
			query += ` AND ($1='' OR p.title ILIKE '%'||$1||'%' OR p.description ILIKE '%'||$1||'%') AND ($2='' OR p.category=$2) AND ($3='' OR p.region=$3 OR p.region='Worldwide')`
			args = append(args, d.Query, d.Category, d.Region)
		}
		query += " ORDER BY p.created DESC LIMIT 100"
		rows, err = a.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p Product
			var btc, xmr int64
			if err = rows.Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Region, &p.Kind, &p.Vendor, &p.VendorID, &btc, &xmr, &p.Stock, &p.Archived); err != nil {
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
		case "orders", "order", "vendor-dashboard", "admin", "disputes":
			query := orderQuery + " WHERE (o.buyer_id=$1 OR p.vendor_id=$1)"
			args := []any{d.User.ID}
			if d.Page == "admin" {
				query = orderQuery + " WHERE true"
				args = nil
			}
			if d.Page == "order" {
				query += " AND o.id=$2"
				args = append(args, r.URL.Query().Get("id"))
			}
			query += " ORDER BY o.created DESC LIMIT 100"
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
			rows, err = a.db.QueryContext(ctx, `SELECT m.id,s.handle,t.handle,m.body,to_char(m.created,'YYYY-MM-DD HH24:MI') FROM messages m JOIN users s ON s.id=m.sender_id JOIN users t ON t.id=m.recipient_id WHERE m.sender_id=$1 OR m.recipient_id=$1 ORDER BY m.created DESC LIMIT 100`, d.User.ID)
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
			rows, err = a.db.QueryContext(ctx, `SELECT id,body,to_char(created,'YYYY-MM-DD HH24:MI'),is_read FROM notifications WHERE user_id=$1 ORDER BY created DESC LIMIT 100`, d.User.ID)
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
		if d.Page == "disputes" || d.Page == "moderator" {
			rows, err = a.db.QueryContext(ctx, `SELECT d.id,d.order_id,d.reason,d.status,d.resolution,to_char(d.created,'YYYY-MM-DD HH24:MI') FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products p ON p.id=o.product_id WHERE o.buyer_id=$1 OR p.vendor_id=$1 OR $2 IN ('admin','moderator') ORDER BY d.created DESC LIMIT 100`, d.User.ID, d.User.Role)
			if err != nil {
				return err
			}
			for rows.Next() {
				var v Dispute
				if err = rows.Scan(&v.ID, &v.OrderID, &v.Reason, &v.Status, &v.Resolution, &v.Created); err != nil {
					rows.Close()
					return err
				}
				d.Disputes = append(d.Disputes, v)
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
			rows, err = a.db.QueryContext(ctx, `SELECT action,to_char(created,'YYYY-MM-DD HH24:MI') FROM audit_events WHERE user_id=$1 OR $2='admin' ORDER BY created DESC LIMIT 100`, d.User.ID, d.User.Role)
			if err != nil {
				return err
			}
			for rows.Next() {
				var e Event
				if err = rows.Scan(&e.Action, &e.Created); err != nil {
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
	o := Order{ID: "sample-draft", ProductID: p[0].ID, Title: p[0].Title, Buyer: u.Handle, Vendor: p[0].Vendor, Currency: "BTC", Amount: p[0].PriceBTC, State: stateDraft, Status: stateLabel(stateDraft), Kind: p[0].Kind, BuyerID: u.ID, VendorID: p[0].VendorID, Created: "Sample order", Updated: "Sample order"}
	return PageData{Page: page, Title: strings.ReplaceAll(strings.Title(page), "-", " "), Preview: true, User: u, Products: p, Product: &p[0], Orders: []Order{o}, Order: &o, Currency: "BTC", Settings: map[string]string{"site_name": "OPSMKT", "bitcoin_mode": "disabled", "monero_mode": "disabled"}, Users: []User{{ID: "ghost", Handle: "ghost_circuit", Role: "vendor"}}, Events: []Event{{Action: "Preview only — no real activity", Created: "—"}}}
}
