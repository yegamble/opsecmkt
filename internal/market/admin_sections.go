package market

import (
	"strconv"
	"strings"
)

// Administration sections (A-542). /admin is an overview; every administration panel is on exactly one section page,
// GET /admin/<slug>. All of them render template page:admin with PageData.Page "admin" (so every admin loader,
// preview and role check applies unchanged) and PageData.AdminSection naming the section ("" on the overview). Each
// admin form keeps its POST path; a successful post returns to the section holding the form. No slug is a
// registered action or raw path (/admin/access, /admin/canary, /admin/payment-intake, /admin/retention/... and
// /admin/audit-export stay as they were).
type AdminSection struct{ Slug, Name, Summary string }

var adminSections = []AdminSection{
	{"", "Overview", "Staff attention, the setup checklist and links to every section."},
	{"users", "Users", "The user directory with each account's details, roles, second-factor resets, suspensions, bans and the staff roster."},
	{"vendors", "Vendors", "Vendor policy, the application queue and the verified-vendor mark."},
	{"payments", "Payments", "Monero escrow status, release deadline, the market fee (read-only), recovery and new escrow checkouts."},
	{"security", "Security & access", "Site access, registrations, the sign-in CAPTCHA and paused sign-ins."},
	{"site", "Site & branding", "Market name, logo and staff contact."},
	{"transparency", "Transparency", "Operator PGP key, warrant canary, public staff list and signed audit export."},
	{"system", "System & audit", "Service isolation and updates, the application host, image scanner, retention, server errors and the audit trail."},
}

// AdminSections is every section after the overview, for the overview's section list.
func (d PageData) AdminSections() []AdminSection { return adminSections[1:] }

// AdminSectionView is the section this page shows (the overview on /admin).
func (d PageData) AdminSectionView() AdminSection {
	for _, s := range adminSections {
		if s.Slug == d.AdminSection {
			return s
		}
	}
	return adminSections[0]
}

// URL is the section's address: /admin for the overview.
func (s AdminSection) URL() string {
	if s.Slug == "" {
		return "/admin"
	}
	return "/admin/" + s.Slug
}

func init() {
	for _, s := range adminSections[1:] {
		registerPage("admin/"+s.Slug, pageSpec{Protected: true, Roles: []string{"admin"}})
	}
}

// adminSectionPage splits a registered page name into the page template and admin section: "admin/users" is page
// "admin", section "users"; any other name is itself with no section.
func adminSectionPage(page string) (string, string) {
	if slug, ok := strings.CutPrefix(page, "admin/"); ok {
		for _, s := range adminSections[1:] {
			if s.Slug == slug {
				return "admin", slug
			}
		}
	}
	return page, ""
}

// adminSectionName is the section's navigation label ("" for an unknown slug).
func adminSectionName(slug string) string {
	for _, s := range adminSections {
		if s.Slug == slug {
			return s.Name
		}
	}
	return ""
}

// AdminSectionAlert is fixed text the overview shows beside a section's link while a warning inside that section
// needs the administrator ("" otherwise), so splitting the page does not hide what the single page showed at a glance.
func (d PageData) AdminSectionAlert(slug string) string {
	var alerts []string
	switch slug {
	case "users":
		if d.BannedOpenEscrow > 0 { // A-652b, STAFF87-7: refund or follow them from each banned account's page
			alerts = append(alerts, strconv.Itoa(d.BannedOpenEscrow)+" banned account(s) have open escrow orders.")
		}
	case "vendors":
		if v := d.VendorAdmin; v != nil && v.QueueTotal > 0 {
			alerts = append(alerts, strconv.Itoa(v.QueueTotal)+" open vendor application(s) await a decision or bond.")
		}
	case "payments":
		if n := d.XMREscrowNode; n != nil && (n.Mismatch != nil || n.NotReady != "" || n.NotSynced != "" || n.TorHold != "") || d.XMREscrowHealth.Attention() {
			alerts = append(alerts, "Monero escrow needs attention.")
		}
	case "security":
		if d.CaptchaNoContact {
			alerts = append(alerts, "The CAPTCHA is on without a staff contact that works without an account.")
		}
	case "system":
		if d.DependencyStatus != nil && d.DependencyStatus.NeedsAttention {
			alerts = append(alerts, "Review required: service isolation or dependency status needs attention.")
		}
		if s := d.Scanner; s != nil && s.Unknown {
			alerts = append(alerts, gatewayScannerUnknown+".")
		} else if s != nil && !s.Disabled && (s.Stale || !s.Ready) {
			alerts = append(alerts, "Photo uploads are paused by the image scanner.")
		}
		if r := d.Retention; r != nil && (r.MaintenanceError != "" || retentionReviewOverdue(r)) {
			alerts = append(alerts, "Retention needs operator attention.")
		}
	}
	return strings.Join(alerts, " ")
}

func retentionReviewOverdue(r *RetentionView) bool {
	for _, h := range r.Holds {
		if h.Overdue {
			return true
		}
	}
	for _, x := range r.AccountExceptions {
		if x.Overdue {
			return true
		}
	}
	for _, x := range r.EscrowExceptions {
		if x.ReviewOverdue {
			return true
		}
	}
	for _, w := range r.WalletCleanup {
		if w.Overdue || w.ReviewOverdue {
			return true
		}
	}
	return false
}

// AdminNavLink is one entry of the administration section navigation.
type AdminNavLink struct {
	URL, Name string
	Current   bool
}

// AdminNav is the section navigation shown on every administration page, the current section marked.
func (d PageData) AdminNav() []AdminNavLink {
	links := make([]AdminNavLink, len(adminSections))
	for i, s := range adminSections {
		links[i] = AdminNavLink{URL: s.URL(), Name: s.Name, Current: d.Page == "admin" && d.AdminSection == s.Slug}
	}
	return links
}

// noticePage is the page name a notice's pages list matches: "admin/<slug>" on an administration section.
func (d PageData) noticePage() string {
	if d.Page == "admin" && d.AdminSection != "" {
		if d.AdminUserHandle != "" {
			return "admin/user" // an account's page in the user directory (A-545)
		}
		return "admin/" + d.AdminSection
	}
	return d.Page
}
