package market

// Shared view types. Each feature package appends fields only inside its own commented block.

type User struct {
	ID, Handle, Role, PGP string
	RetentionRestricted   bool   // only existing obligations and sign-in security while closure waits for funds
	RetentionContinuity   bool   // sole administrator may appoint a replacement while closure waits
	Factors               string // P1, PageData.FactorAccounts only: enrolled second factors, e.g. "TOTP and PGP sign-in"
	Suspended             string // P1, PageData.SuspendedAccounts only: when the account was suspended
	Avatar                string // A-189, vendor page and /canary staff list only: the avatar thumbnail on /img, "" when none may be shown
	VendorVerified        bool   // A-544, vendor page only: an administrator marked this vendor verified
	// A-546, PageData.BannedAccounts only: when the account was banned, by which administrator ("" once that account
	// is erased) and why.
	Banned, BannedBy, BanReason string
}
type Product struct {
	ID, Title, Description, Category, Region, Kind, Vendor, VendorID, PriceBTC, PriceXMR string
	BTCOrderUnavailable, XMROrderUnavailable                                             string // legacy price below that currency's escrow settlement minimum
	Stock                                                                                int
	Archived                                                                             bool // Foundation; P4 uses
	VendorVerified                                                                       bool // A-544: an administrator marked the vendor verified
	// P4, vendor-dashboard only: an administrator's takedown reason (A-163); never loaded for public pages.
	TakedownReason string
	// A-189, catalog, vendor and product pages only: the listing's public photos, oldest first (image_pages.go),
	// and Thumb, the first one's thumbnail URL for its card ("" = the CSS illustration).
	Photos []ProductPhoto
	Thumb  string

	// Vendor storefront controls apply to public listings and existing-order views alike.
	VendorVacation, VendorDeactivated, VendorMessagesUnavailable bool
}

// ProductPhoto is one public photo of a listing on the product page (A-189): its URLs on the public image route,
// its size and description, and the text naming it: Alt for photo 1, shown full size, and Label for the link to
// each other photo's full size.
type ProductPhoto struct {
	ID, Full, Thumb, Description, Alt, Label string
	Width, Height                            int
	// A-190, product page only: the staff remove form (image_takedown.go), nil unless the viewer is staff.
	Takedown *ImageTakedown
}

// Order.Status is the display label derived from State by stateLabel.
type Order struct {
	ID, ProductID, Title, Buyer, Vendor, Currency, Amount, Status, Created string
	State, Kind, BuyerID, VendorID, Updated                                string
	// Listing snapshot is captured only when the draft first requests payment. Kind remains
	// operational (live for earlier orders with no snapshot); KindDisplay never invents
	// historical terms for those orders. Description is rendered only on an authorized order.
	KindDisplay, Description string
	ListingRecorded          bool
	// Escrow: settled through a per-order escrow instead of the pooled wallet (settleTerminal, escrowOnly): the order
	// has an escrows row (WR-escrow Phase 1, escrow_create.go), of either coin (btcEscrow).
	Escrow bool
}
type Message struct {
	ID, Sender, Recipient, Body, Created string
	// P2 PGP
	Encrypted      bool
	RecipientMatch string
	// Count is nullable at rest: legacy rows or an unknown recipient key cannot support a send-time count. It counts
	// unique visible recipient key IDs, excluding the recipient's saved IDs; hidden IDs are not included.
	RecipientExtraKeys    int
	RecipientExtraKeysSet bool

	CanReply bool // current recipient policy permits replying to this incoming message
}
type Notification struct {
	ID, Body, Created, OrderID string
	Read                       bool
}

// StaffContactView is the administrator's staff contact (A-119, staff_contact.go): Text as saved, shown escaped;
// Handle set only when a signed-in reader may be linked to /messages?to=<Handle>.
// NeedsAccount: the contact is a bare handle, which only a signed-in user can message (A-517).
type StaffContactView struct {
	Text, Handle string
	NeedsAccount bool
}
type Dispute struct {
	ID, OrderID, Reason, Status, Resolution, Created string
	NoResolver                                       bool // open, and no eligible resolver exists (eligibleResolverSQL)
	// A-124: Opener is "buyer" or "vendor", the party who moved the order to disputed ("" when no such event is
	// recorded). Reply is the other party's one reply (an order_events row, disputeReplyPrefix), ReplyRole its
	// author's role and ReplyCreated its time.
	Opener, Reply, ReplyRole, ReplyCreated string
}

// Responder is the party who may reply to the dispute: the one who did not open it ("" when the opener is unknown).
func (d Dispute) Responder() string {
	switch d.Opener {
	case roleBuyer:
		return roleVendor
	case roleVendor:
		return roleBuyer
	}
	return ""
}

// EvidenceHolder (A-162): a moderator or administrator a party messaged since the dispute opened, with how many
// messages and the time of the latest, read from message metadata (sender, recipient, time) only. Suspended: that
// account is suspended now, so it cannot sign in or answer until restored (A-280). Former: that account was staff
// when messaged and has since been removed from staff, as its role-change audit row records (A-294).
type EvidenceHolder struct {
	Handle, Latest    string
	Count             int
	Suspended, Former bool
}

// DisputeEvidence lists the staff each party of an open dispute messaged since it opened.
type DisputeEvidence struct{ Buyer, Vendor []EvidenceHolder }

// PaymentReview is one deposit the payment watcher flagged for staff review (payments.flagged) with no recorded
// disposition, or one escalated to an administrator (A-31). Open (paymentFlagOpenRule): a locked transfer, a late
// deposit or a deposit of a payout below the minimum, a credited deposit in an announced regression that has not
// confirmed again, or an escrow deposit recorded after the order closed, left out of its settlement (credited or not)
// or spent outside the market; an escrow deposit that counts for the order is never open (its release or refund
// settles it). Reason names which, the escrow ones included (loadPaymentReviews).
// ID is payments.id. Party: the viewer is the order's buyer or vendor. Form: the viewer may record the disposition
// (open, not a party, and an administrator when Escalated). Refundable: not credited and not an escrow deposit, so
// "refunded by hand" applies (A-330: an escrow deposit is returned by a stray refund). Escrow: an escrow order's deposit.
type PaymentReview struct {
	ID, OrderID, OrderState, Currency, Amount, TxID, Reason, Flagged string
	ProductID, Title                                                 string // the order's listing, linked from the row (A-265)
	Index                                                            int64
	Open, Escalated, Party, Form, Refundable, Escrow                 bool
}
type Event struct{ Handle, Action, Created string } // Handle: account the audit row is recorded on ("" = system)

// SignInRefusal is a current, in-memory handle lockout on this application instance.
// It is visible only on the administrator's sign-in protection panel.
type SignInRefusal struct{ Handle, Until string }

type PageData struct {
	// AdminSection is the administration section page /admin/<slug> shown with Page "admin"; "" is the overview
	// (admin_sections.go).
	AdminSection     string
	Access           *SiteAccessView
	Branding         *BrandingView
	DependencyStatus *DependencyStatusView // Administrator-only local version report and topology notices.
	HostControl      *HostControlView      // Private administrator app-host reboot readiness and receipts.

	ServerStarted string
	SchemaVersion int
	Retention     *RetentionView

	StaffRoster       []StaffRosterEntry
	StaffRosterPaging *ActivityPaging

	RecentServerErrors []ServerErrorEntry // administrator-only, volatile, no request contents

	Page, Title, CSRF, Error, Notice, Query, Category, Region, Currency, Mode string
	NoticeAt                                                                  string // the section a one-shot notice renders in, "" = the top (notice.go)
	HeaderCSRF                                                                string // the header sign-out's token when User came from the session cookie on a refusal page (A-548)
	User                                                                      *User
	Products                                                                  []Product
	Product                                                                   *Product
	Orders                                                                    []Order
	Order                                                                     *Order
	Messages                                                                  []Message
	Notifications                                                             []Notification
	Disputes                                                                  []Dispute
	Concession                                                                *ConcessionPreview  // disputed order: current party's one final concession
	DisputePartyCounts                                                        *DisputePartyCounts // /disputes: only this account's own orders
	Users                                                                     []User
	Events                                                                    []Event
	Settings                                                                  map[string]string
	Preview                                                                   bool
	AuditHandle                                                               string // /admin audit trail's exact-handle filter.
	// Paging: catalog and public vendor page of the matching listings (A-181, A-189, load.go).
	Paging *CatalogPaging
	// ActivityPaging: signed-in user's orders, messages or notifications (A-16, load.go).
	ActivityPaging *ActivityPaging
	// A-545 (admin_users.go). AdminUserHandle: /admin/users?handle=, the account whose page is shown (set from the
	// request before the notice is taken, so an account-page notice renders there); UserDirectory: the directory on
	// /admin/users; AdminUser: the account page.
	AdminUserHandle string
	UserDirectory   *UserDirectoryView
	AdminUser       *AdminUserView
	// DeliveryCleanup lists only this account's archived listings that still hold automatic-delivery text; the text itself is never loaded.
	DeliveryCleanup       []DeliveryCleanupListing
	DeliveryCleanupPaging *ActivityPaging
	DeliveryCleanupCount  int
	// UploadBack and UploadBackLabel: the refusal page of an upload (A-186, uploads.go) links back here.
	UploadBack, UploadBackLabel string
	// Refusal: the shared refusal page of a refused request (A-514, refusal.go); nil on every other page.
	Refusal *RefusalView
	// FormHandle: /login and /register re-rendered after a refused post, the handle that was typed (A-514).
	FormHandle string
	// FormVendor: /register re-rendered after a refused post that chose a vendor account (A-544).
	FormVendor bool
	// A-544 vendor onboarding (vendor_applications.go). VendorPolicy: /register, the policy a vendor sign-up follows;
	// VendorAccount: /account for a buyer or vendor; VendorAdmin: the admin vendors panel.
	VendorPolicy  *VendorPolicyView
	VendorAccount *VendorAccountView
	VendorAdmin   *VendorAdminView
	// VendorAvailability: own storefront settings on /account, public availability on /vendor.
	VendorAvailability *VendorAvailabilityView
	// PGPAccessVerified: this session proved decryption with the account's current public key.
	PGPAccessVerified bool
	// ImageStorage: admin page, stored photos and avatars against IMAGE_STORAGE_LIMIT_MB (uploads.go).
	ImageStorage *ImageStorageView
	// A-189 (image_pages.go). StaffListPublic: /canary, the administrator's "Show a public staff list" is on;
	// StaffList: then the moderators and administrators listed (User.Avatar set when it may be shown).
	StaffListPublic bool
	StaffList       []User
	// StaffContact: /canary, /challenge, /login, /register, /notifications, /vendor-dashboard and /listing-edit (a
	// taken-down listing, A-279), the administrator's staff contact (A-119, staff_contact.go); nil while unset.
	StaffContact *StaffContactView
	// CaptchaNoContact: admin page, the CAPTCHA is required but no staff contact works without an account (A-517).
	CaptchaNoContact bool
	// StaffAttention: uncapped actionable category totals in the shared navigation and staff desk summaries.
	StaffAttention *StaffAttention
	// A-190 (image_takedown.go). RecentImages: /moderator, the newest public photos and avatars (vendors' only for a moderator); AvatarTakedown:
	// vendor page, the staff remove form for the vendor's avatar.
	RecentImages   []RecentImage
	AvatarTakedown *ImageTakedown

	// Foundation
	Auth *AuthView

	// P1 Authentication
	Security *SecurityView
	Captcha  *CaptchaView
	// Next and NextTitle: /login and /register, the page signing in returns to and its title; /totp after a
	// recovery-code sign-in, the page it links on to (A-126, signInNext).
	Next, NextTitle string
	// Sessions: account page, the viewer's live sessions, this browser's first, then the newest others, at most
	// sessionsListed of SessionsTotal (A-157, actions_account.go).
	Sessions      []AccountSession
	SessionsTotal int
	// FactorAccounts: admin page, up to 100 non-administrator accounts with a second factor, by handle
	// (User.Factors names them); the reset form itself takes any handle.
	FactorAccounts []User
	// SuspendedAccounts: admin page, up to 100 suspended accounts, most recently suspended first
	// (User.Suspended says when); the suspend/restore form itself takes any handle.
	SuspendedAccounts []User
	SignInRefusals    []SignInRefusal
	// AdminIngressRefusals: admin page, the handles paused on the private administrator ingress, whose sign-in
	// budgets are separate from the public listener's (A-570).
	AdminIngressRefusals []SignInRefusal
	RecentSignIns        []Event // account only: latest five successful sign-ins, including legacy audit rows.
	// BannedAccounts: admin page, up to 100 banned accounts, most recently banned first (A-546; User.Banned, BannedBy
	// and BanReason); the ban/unban form itself takes any handle.
	BannedAccounts []User
	// RoleHandle, RoleChoice, ResetHandle, SuspendHandle and SuspendChoice: admin page re-rendered after a
	// handle matched no account, keeping what was typed into the role, second-factor reset or suspension form.
	RoleHandle, RoleChoice, ResetHandle, SuspendHandle, SuspendChoice string
	// BanHandle, BanChoice and BanReason: likewise for the ban/unban form (A-546).
	BanHandle, BanChoice, BanReason string
	// ResolveDispute and ResolveText: the desk re-rendered after a resolve form loaded before the other party's
	// reply was refused (A-280), keeping the decision typed for that dispute.
	ResolveDispute, ResolveText string
	DisputePartyFilter          string // moderator desk's exact handle filter (?party=), shown only to staff.

	// P2 PGP
	PGP *PGPView

	// P3 Orders
	OrderEvents   []OrderEvent
	Transitions   []Transition
	Delivery      *DeliveryView
	Reviews       []Review
	ReviewSummary *ReviewSummary
	CanReview     bool
	// IncomingOrders: all active orders and bounded terminal history on the viewer's own listings.
	// NeedsAction counts all paid orders; VendorOpenDisputes counts open disputes on those listings.
	IncomingOrders     []Order
	NeedsAction        int
	VendorOpenDisputes int
	// DeadlineBanners: /orders, the viewer's escrow orders whose release deadline is less than 72 hours away
	// (escrow_deadline.go, P2-R9).
	DeadlineBanners []DeadlineBanner
	// DisputeOrders: order summary per dispute (disputes and moderator pages), keyed by order id.
	// OpenDisputes: how many leading entries of Disputes are open (the rest are resolved).
	DisputeOrders map[string]Order
	OpenDisputes  int
	// DisputePayouts: resolve form per open dispute the viewer may resolve, keyed by dispute id.
	DisputePayouts map[string]PayoutPreview
	// DisputeEvidence: per open dispute a staff viewer is not party to, keyed by dispute id, whom each party messaged
	// since it opened (A-162). Absent (nil) for everyone else.
	DisputeEvidence map[string]*DisputeEvidence
	// Overpaid: order page for its buyer or vendor, an escrow order not settled yet (paid, shipped, delivered or
	// disputed) holding more than the price: what a release and a refund do with the excess (escrowOverpaid, A-508);
	// "" otherwise.
	Overpaid string
	// HistoryLimit: non-zero when closed history (resolved disputes, or completed/resolved/funded-cancelled
	// vendor orders) was cut to this many most recent rows.
	HistoryLimit int
	// DeliveryWithheld: order page viewed by a reviewer of a payment flag on an order never disputed.
	DeliveryWithheld bool
	// PaymentReviews: moderator desk, payments flagged for review: every open flag (PaymentReviewsOpen of them,
	// oldest first), then the most recent other flags (PaymentReviewOthers in all); PaymentReviewLimit is
	// non-zero when the other flags were cut to that many rows.
	PaymentReviews      []PaymentReview
	PaymentReviewsOpen  int
	PaymentReviewOthers int
	PaymentReviewLimit  int
	// EscrowAlerts: moderator desk, escrow orders with an open staff alert (escrowStaffAlert, A-470), newest first, at
	// most escrowAlertLimit; EscrowAlertsMore is how many more there are than it lists (QA5W1-2).
	EscrowAlerts     []EscrowAlertView
	EscrowAlertsMore int
	// StaffAlert: order page, set when staff read the order only because its escrow has an open staff alert (no
	// deposit flag, no dispute): which alerts are open, so the page names them (QA5W1-1).
	StaffAlert *EscrowAlertView
	// XMRCollisions: order page, for staff who are not parties only, the order's Monero escrow deposits refused as
	// another order's escrow account (A-480), oldest first, each with its disposition or its form.
	XMRCollisions []XMRCollisionView
	// XMRCollisionForm: one of XMRCollisions is open, so the page shows its disposition form (the staff help then
	// names it as the one thing staff change on the page, QA5W3B-6).
	XMRCollisionForm bool
	// XMRSettlementForm: order page, the newest Monero escrow settlement shown with an administrator's form (re-request
	// or build again, escrow_xmr_settle_view.go), 0 for none; the staff help then names and links it (QA5W4B-7).
	XMRSettlementForm int64
	// XMRSettlementActions: the administrator's settlement forms the order page offers, named for the staff help
	// ("re-request it or mark it settled by hand"; xmrSettlementActions, A-613).
	XMRSettlementActions string

	// P4 Inventory (uses Product.Archived)
	Listing   *ListingView
	Inventory *InventoryView
	// VendorReadiness: the seller's current listing, payment and settlement prerequisites, without wallet details.
	VendorReadiness *VendorReadinessView

	// P5 Payments
	Providers         []ProviderStatus
	PaymentsEnabled   bool
	PaymentNetworks   string
	LiveNetwork       bool   // payment providers are on mainnet: amounts are real funds
	BitcoinPriceLabel string // network-aware label for a BTC listing price, including an unknown state.
	// EscrowOn: ESCROW_BTC=on (every page); EscrowAvailable: escrow passed its checks. EscrowOrders: vendor desk, the
	// viewer's escrow orders that may hold coins (QA31-2): the site-wide notices never say no funds are held beside them.
	// EscrowNet: Bitcoin escrow's chain, regtest, signet or testnet4 (App.escrowNetwork, A-443; every page), so the copy
	// names it (partials/escrow-net.html); "" while it is not verified yet (App.escrowNetKnown, QA4W6-8), and the copy
	// says so.
	EscrowOn, EscrowAvailable bool
	EscrowNet                 string
	// DispatchOn: ESCROW_BTC or ESCROW_XMR on (every page): the listing forms offer the dispatch window, which both coins'
	// physical escrow orders snapshot (physicalEscrowOff).
	DispatchOn   bool
	EscrowOrders int
	// XMREscrowOn: ESCROW_XMR=on (every page), the site-wide notices describe Monero escrow (A-423); XMREscrowAvailable:
	// it is ready to take a checkout (xmrEscrowReady). XMRCustody is xmrCustody (every page), so a template never
	// retypes the owner's sentence (A-402); XMREscrowOrders: vendor desk, the viewer's Monero escrow orders that may hold
	// coins, counted like EscrowOrders. XMREscrowNet: Monero escrow's network, "fakechain", "stagenet" or
	// "mainnet" (App.xmrEscrowNetwork, A-449, A-460), so the notices name it. XMREscrowNetKnown: that network was verified (escrowXMR.formNet
	// is not ""), so /admin's Payment providers text never names a network no daemon was verified on (QA4W4B-1).
	XMREscrowOn, XMREscrowAvailable, XMREscrowNetKnown bool
	XMRCustody, XMREscrowNet                           string
	XMREscrowOrders                                    int
	// EscrowKey: /escrow-key and the /account status row, the viewer's Bitcoin escrow key; nil while ESCROW_BTC is off
	// (escrow_account.go, WR-escrow Phase 1).
	EscrowKey *EscrowKeyView
	// EscrowFee: /admin while ESCROW_BTC is on, the Bitcoin escrow market fee in force and its form (escrow_fee.go)
	EscrowFee *EscrowFeeView
	// EscrowNode: /admin while ESCROW_BTC is on and the escrow node is not synced or not on the pinned chain (A-439)
	EscrowNode *EscrowNodeView
	// XMREscrowNode: /admin while ESCROW_XMR is on and the escrow daemon is not synced or not on the pinned network
	// (A-449; Mismatch names networks, not chains), or Monero escrow is not ready for another reason (NotReady, QA4W4B-1)
	XMREscrowNode *EscrowNodeView
	// XMREscrowHealth: /admin while ESCROW_XMR is on, the Monero escrow watcher's persisted health (A-553, STAFF66-3)
	XMREscrowHealth *XMREscrowHealthView
	// EscrowSigner: /admin while ESCROW_SIGNER_SOCKET is set and the escrow signer is not ready (A-454)
	EscrowSigner *EscrowSignerView
	// EscrowReleaseDays: /admin while ESCROW_BTC is on, the release deadline length in force and its form (escrow_release_admin.go)
	EscrowReleaseDays *EscrowReleaseDaysView
	// XMRFee: /admin, the Monero escrow market fee set on the server, read-only (escrow_xmr_fee.go, A-550)
	XMRFee *XMRFeeView
	// EscrowRecovery: /admin, the Bitcoin escrow restore gate, held settlements and orphan addresses (escrow_recovery.go)
	EscrowRecovery *EscrowRecoveryView
	// XMREscrowRecovery: /admin, Monero escrow settlements held by a restore and destroyed wallets' files
	// (escrow_xmr_restore.go)
	XMREscrowRecovery *XMREscrowRecoveryView
	// Escrow: order page of an escrow order, its escrow (escrow_order.go); EscrowCheckout: order page of the buyer's
	// BTC draft on a listing that takes escrow, while ESCROW_BTC is on. Both nil otherwise.
	Escrow         *EscrowOrderView
	EscrowCheckout *EscrowCheckoutView
	// EscrowSetup: /escrow-setup, a per-order escrow's order wallet setup for one of its two parties only (escrow_setup.go,
	// A-456b); nil otherwise.
	EscrowSetup *EscrowSetupView
	// XMREscrow: order page of a Monero escrow order, its escrow (escrow_xmr_order.go); XMREscrowCheckout: order page of
	// the buyer's XMR draft on a listing that takes escrow, while ESCROW_XMR is on. Both nil otherwise.
	XMREscrow         *XMREscrowOrderView
	XMREscrowCheckout *XMREscrowCheckoutView

	// P6 Transparency
	Canary      *CanaryView
	AuditExport *AuditExportView

	// Scanner: admin page, the image scanner panel (clamd.go)
	Scanner *ScannerView
	// Uploads: listing-edit's Photos panel and /account's avatar panel and staff list choice (upload_forms.go)
	Uploads *UploadsView

	// Contacts (contacts.go): counterparty PGP keys, message recipient prefill, unread notifications
	Contacts                []ContactKey // vendor page: the vendor; order page: the other party (both for a reviewer); messages: the ?to recipient
	StaffContacts           []ContactKey // disputed order: independent moderators and administrators
	MessageTo               string       // messages page: validated ?to handle for the recipient field
	MessageLookupPaused     bool         // messages page: the account's recipient lookup budget is spent (A-516); no key, no existence text
	MessageBody             string       // messages page: the ciphertext kept in the compose form after a hidden-recipient send (A-569)
	MessageHiddenRecipients bool         // messages page: ask the sender to confirm a message whose recipient key IDs are all hidden (A-569)
	OrderViewer             string       // order page: "buyer", "vendor" or "moderator" (read-only dispute review)
	OrderSuspendedParties   []string     // current buyer/vendor suspension status; never the administrator's reason
	UnreadNotifications     int          // signed-in users, every page
	// VendorKeyUnusable (A-83/A-225): a physical listing's product and checkout pages, and its buyer's order page, when
	// the vendor has no key that can receive encrypted messages (canReceiveEncrypted); on a paid physical order it
	// also tells the vendor to update their key before buyers send shipping addresses.
	VendorKeyUnusable bool
	// PurchaseBlocks (A-444): a listing's product and checkout pages, each coin whose intake is escrow only while the
	// listing cannot use its escrow (escrow_readiness.go), PurchaseBlocked the same by coin, and NoPurchase when every
	// coin is blocked: the listing is not available for purchase.
	PurchaseBlocks  []PurchaseBlock
	PurchaseBlocked map[string]bool
	NoPurchase      bool
	// A-109: product/checkout and a buyer's draft-order page explain why a suspended vendor takes no new intake.
	VendorSuspensionReason string
	// SellerClosed (A-546): the product page of a banned vendor's listing (archived by the ban) says the account is
	// closed rather than only archived; the vendor page says so instead of offering "Message vendor" and the key.
	SellerClosed bool
	// OrderClosedParties (A-546): like OrderSuspendedParties, for a banned buyer or vendor (not also listed there).
	OrderClosedParties []string
	// EscrowReadiness (A-444): /admin, the escrow-only readiness panel (escrow_readiness.go).
	EscrowReadiness *EscrowReadinessView
	// VendorEscrowKey (A-455, P4-R18): the product and vendor pages' vendor's Bitcoin escrow key type while ESCROW_BTC is
	// on: "core", "per_order" or "none" (no active key); "" with escrow off.
	VendorEscrowKey string
	// noticeCode: the one-shot notice this render consumed (notice.go), for a loader that details it from the
	// current state (/totp after a recovery-code sign-in); "" without one.
	noticeCode string
}

// Foundation
type AuthView struct{ TOTPEnrolled, PGPEnrolled bool }

// P1 Authentication
type SecurityView struct {
	TOTPEnabled               bool
	PendingSecret, OTPAuthURI string
	RecoveryCodes             []string // plaintext, only on the single view right after issuing
	Pending                   bool     // enrollment started, not yet confirmed with a code
	RecoveryRemaining         int
	Error                     string
}

// AccountSession is one live session on /account: when it started ("2006-01-02 15:04 UTC") and whether it is
// this browser's. The market records nothing else about a session's client.
type AccountSession struct {
	Started string
	Current bool
}

// CaptchaView is set on login/register while the CAPTCHA is required; ID is empty when none could be issued (Error says why).
type CaptchaView struct{ ID, Error string }

// P2 PGP
type PGPView struct {
	Fingerprint                               string
	Verified                                  bool
	VerifiedAt, Challenge, EncryptedChallenge string
	TwoFactor                                 bool
	HasKey                                    bool     // a public key is saved on the profile
	Armored                                   string   // the saved key for the owner's form: canonical when it parses, else as stored
	UserIDs                                   []string // user IDs of the saved key, shown to the owner (A-79)
	KeyError                                  string   // the saved key no longer parses (legacy or revoked)
	ChallengeExpires                          string   // open ownership challenge expiry (UTC)
	// A-155: expiry of the primary key and of the encryption subkey encryptTo selects (or the one that expired last),
	// "Never", "YYYY-MM-DD UTC" or "Expired YYYY-MM-DD UTC"; SubkeyExpires is "" when no subkey can encrypt.
	PrimaryExpires, SubkeyExpires string
	StopsSoon                     string // A-155: UTC date encryption to the key stops, when that is within 30 days
	FingerprintHex                string // unspaced fingerprint for the renewal commands
	CannotEncrypt                 bool   // A-225: nothing can be encrypted to the saved key now (canReceiveEncrypted)
}

// P3 Orders
// OrderEvent.Avatar (A-189, owner decision 1(c)): on a dispute decision, the deciding staff member's avatar
// thumbnail on /desk/img when the viewer may see it there; "" otherwise.
type OrderEvent struct{ From, To, Actor, Note, Created, Avatar string }
type Transition struct {
	To, Label, Action string         // Action "" = shown as unavailable with Label as the reason
	Payout            *PayoutPreview // cancel of an escrow order holding coins: the refund it requests
}

// PayoutPreview states the settlement a form requests from an escrow (a cancel of an order holding coins, resolve;
// A-161, A-122): Seen is the amount escrowSettleTerminal counts, in atomic units, sent back as amount_seen; Confirm
// labels the required confirmation checkbox. Resolve form only: Release and Refund state each outcome's settlement
// (escrowDisputePreview: amount, recipient, destination, what happens to an amount paid above the price); Escrow, when
// set, how a Monero escrow order's decision is paid (xmrDisputePreview).
type PayoutPreview struct {
	Seen                             int64
	Confirm, Release, Refund, Escrow string
}

type ConcessionPreview struct {
	Seen                        int64
	Outcome, Statement, Confirm string
}

type DisputePartyCounts struct{ Open, Closed, Withdrawn int }

type DeliveryView struct{ Content, Created string }
type Review struct {
	ID, OrderID, Buyer string
	Rating             int
	Body, Created      string
	Product            string // listing title, vendor page only
	Hidden             bool   // hidden by an administrator (A-125): listed only for administrators and on the order
}

// ReviewSummary counts and averages the public (not hidden) reviews; Hidden counts the others, for administrators only.
type ReviewSummary struct {
	Count   int
	Average string
	Hidden  int
}

// P4 Inventory
// ListingView is the owner/admin-only edit view of one listing (d.Product holds the public fields).
type ListingView struct {
	DeliveryContent     string // stored unencrypted; loaded only for the listing's own vendor
	VendorEditor        bool   // the editor is the listing's vendor (not an administrator editing another vendor's listing)
	HasDeliveryContent  bool   // automatic delivery content is stored (shown to other editors instead of the content)
	Updated, ArchivedAt string
	OpenOrders          int // orders not completed, resolved or cancelled
	Reservation         ListingReservation
	AutoDelivery        string // currencies with a configured payment provider, e.g. "BTC, XMR"; "" = none
	TakedownReason      string // an administrator's takedown reason (A-163); "" when not taken down
	DispatchDays        int    // a physical listing's dispatch window in days (P2-R5); the default shown when none is set
}
type InventoryView struct {
	Active, Archived, Total        int
	AutoDelivery                   string
	Filter                         string
	AllURL, ActiveURL, ArchivedURL string
	Paging                         *InventoryPaging
	Reservations                   map[string]ListingReservation
}

// ListingReservation is a vendor-only summary of units held by non-draft, nonterminal orders and
// the next payment-window expiry among awaiting-payment orders.
type ListingReservation struct {
	Units         int
	PaymentExpiry string
}

type InventoryPaging struct {
	Page, Pages, Total, First, Last int
	PrevURL, NextURL                string
}

func (p InventoryPaging) Offset() int   { return (p.Page - 1) * inventoryPageSize }
func (p InventoryPaging) PrevPage() int { return p.Page - 1 }
func (p InventoryPaging) NextPage() int { return p.Page + 1 }

// P5 Payments
type PaymentDeposit struct {
	TxID          string
	Index         int64
	Amount        string
	Confirmations int64
	State         string
}

// PurchaseBlock (A-444) is why a listing cannot be bought in Currency while its intake is escrow only, and Fix what
// its vendor can do (shown to the vendor only).
type PurchaseBlock struct{ Currency, Reason, Fix string }

// EscrowReadinessView is /admin's escrow-only readiness panel (A-444): per coin its intake and whether its escrow is
// on, the active listings that cannot use escrow and the vendors with active listings but no active Bitcoin escrow
// key. ListingCount and VendorCount are uncapped; Listings and Vendors hold at most Limit rows.
type EscrowReadinessView struct {
	Coins                     []ReadinessCoin
	Listings                  []ReadinessListing
	Vendors                   []ReadinessVendor
	ListingCount, VendorCount int
	Limit                     int
}

// ReadinessCoin: Intake is payment_intake_<COIN>; NotReady, why the coin's escrow, on, is not ready ("" when it is,
// QA4W4B-1).
type ReadinessCoin struct {
	Currency, Intake, NotReady string
	EscrowOn                   bool
}
type ReadinessListing struct{ ID, Title, Vendor, Kind, Reason string }

type ReadinessVendor struct {
	Handle   string
	Listings int
}

// VendorReadinessView is the vendor dashboard's concise view of what the current intake modes can accept.
type VendorReadinessView struct {
	EncryptionUsable bool
	Currencies       []VendorCurrencyReadiness
}

type VendorCurrencyReadiness struct {
	Currency, Intake, Provider, Escrow, Checkout string
	EscrowKeyRequired, EscrowKeyActive           bool
}

type ProviderStatus struct {
	Currency, Network string
	Enabled           bool
	Configured        bool   // the pooled wallet is configured in this process, available or not (pooledConfigured)
	Status            string // Enabled, Unavailable (retried), Refused, Disabled
	Error             string
	Confirmations     int
	Version           string // wallet or node version its last check read (versionReporter)
	VersionWarning    string // set when send errors are all ambiguous because Version is not the verified one
	// AddressCollisions: payment requests refused because the wallet returned an address already issued (A-288),
	// since an administrator last dismissed the warning; AddressCollisionLast: when the latest one was refused.
	AddressCollisions    int
	AddressCollisionLast string
}

// P6 Transparency
type CanaryView struct {
	Statement, Fingerprint, SignedAt, Posted, Error string
	Verified                                        bool
	// Clearsigned is the stored submission exactly as posted; OperatorKey is the configured armored public key
	// (both published so readers can verify independently). KeyError explains an unusable configured key.
	Clearsigned, OperatorKey, KeyError string
}
type AuditExportView struct {
	Available bool
	Encrypted bool // downloads require offline recipient-key decryption before signature verification
	PublicKey string
	UpTo      int64
	Error     string // why the signed export is unavailable
}

// Contacts
// ContactKey is another user's saved PGP public key as shown to people who need to encrypt to them.
// Fingerprint is empty when no usable key is saved; Verified only when the ownership proof matches this key.
type ContactKey struct {
	Handle, Relation     string // Relation: "vendor", "buyer" or "" (message recipient)
	Armored, Fingerprint string
	Verified             bool
	VerifiedAt           string // UTC date of the ownership proof (date only for other users, A-79)
	Unreadable           bool   // a key is saved but no longer parses (legacy or revoked)
	NoEncryption         bool   // a key is saved and parses, but nothing can be encrypted to it now (A-68: expired, revoked or signing-only)
	ChangedAfterOrder    string // A-158: UTC date the vendor's key was changed or re-verified after the paid physical order was placed (buyer's view)

	CanMessage bool // the current viewer may send encrypted messages to this contact
}

// Usable reports whether the contact has a key that can receive encrypted messages (contactKey fills Fingerprint only then).
func (k ContactKey) Usable() bool { return k.Fingerprint != "" }

// EscrowOrderView is an escrow order's escrow on its order page (escrow_order.go). Descriptor is the bare-key
// descriptor with its checksum, the viewer's own key with its origin (Own names the viewer's party; "" for a staff
// reader, who sees every key bare). RefundAddress is shown to the buyer only. Status and NextStep are fixed copy.
type EscrowOrderView struct {
	Available                                   bool // the escrow node is up, so the escrow is being watched
	Download                                    bool // ESCROW_BTC=on: /escrow-descriptor exists
	Address, Descriptor, Own, RefundAddress     string
	Required, Confirmed, Unconfirmed, Remaining string
	Threshold                                   int64
	Open, NeedsTopUp, ConfirmationWait          bool
	WindowPassed                                bool // Open, CancelAfter has passed, no deposit waits for confirmations and the escrow node is not synced: the watcher holds the order (QA4W1-5)
	CancelAfter, Status, NextStep, Fee          string
	Deposits                                    []PaymentDeposit
	Unread                                      string                  // set when nothing is read at the address: shown for the Confirmed and Unconfirmed figures (A-338)
	Settlement                                  *EscrowSettlementView   // the order's claim or refund (escrow_settle.go)
	Settlements                                 []*EscrowSettlementView // Settlement (when set) then the stray refunds, newest first, as the page lists them
	Decision                                    *EscrowDecisionView     // a resolved order's dispute decision (escrow_decision.go)
	Claim                                       bool                    // the vendor of a completed order may request the claim
	Recover, RecoverAsked                       bool                    // the buyer of a closed order may ask to recover coins at the escrow (or has asked; escrow_stray.go)
	Held                                        bool                    // a closed order's coins a stray refund returns are held: too few after its last one failed (strayHeldSQL, A-434)
	RestorePaused                               bool                    // the restore gate pauses escrow builds, co-signing and broadcasts (escrow_recovery.go)
	PartiesOnly                                 bool                    // a shipped order's release: only both parties' signatures complete it (escrow_release.go)
	Deadline                                    *EscrowDeadlineView     // a shipped order's release deadline (escrow_deadline.go)
	RangePath                                   string                  // the viewer's child key in this escrow, set only past escrowRangeAdvice: the page advises keypoolrefill (A-397)
	KeypoolSize                                 int64                   // the keypoolrefill size it advises: 1000 keys past that child
	PerOrder                                    bool                    // a per-order (shape (b)) escrow: its parties sign in an order wallet (escrow_setup.go, A-456b)
}

// EscrowSetupView is what one party of a per-order (shape (b)) escrow builds its order wallet from (escrow_setup.go,
// A-456b): its own key's origin with the per-order index ([fingerprint/path/i]) and that path as a wallet asks for it
// (m/path/i), the two cosigner keys served rooted (the other party's per-order key and the market's, as Vpubs), the first
// receiving address the wallet must show (the escrow address) and a Sparrow output descriptor of the order wallet.
// Unavailable says why the cosigner keys cannot be shown; NoSparrow why the descriptor cannot (a stored order wallet key
// cannot be read: a row from before it was stored, QA5W4A-5 and QA5W5A-1).
type EscrowSetupView struct {
	OrderID, Short, Own    string
	OwnPath, OwnDerivation string
	Cosigners              []EscrowCosignerView
	Address                string
	Sparrow                string
	Unavailable, NoSparrow string
}

// EscrowCosignerView is one cosigner key on the setup page: whose it is and the key, rooted, as a Vpub.
type EscrowCosignerView struct{ Who, Key string }

// EscrowDeadlineView is a shipped escrow order's release deadline on its page (escrow_deadline.go, P2-R9): At in UTC,
// Left the time left when the page loaded; Due once it has passed; Rechecked when a restore from backup moved it.
type EscrowDeadlineView struct {
	At, Left       string
	Due, Rechecked bool
}

// DeadlineBanner is one /orders banner (escrow_deadline.go): an escrow order of the viewer's whose release deadline At
// (UTC) is less than 72 hours away, Left as computed when the page loaded; Buyer when the viewer is its buyer; XMR for a
// Monero escrow order (escrow_xmr_deadline.go).
type DeadlineBanner struct {
	OrderID, Short, At, Left string
	Buyer, XMR               bool
}

// EscrowSettlementView is an escrow order's settlement on its page (escrow_settle.go): its state in honest words, its
// frozen outputs, and for a party while it awaits a signature the party's own copy (PSBT, base64) and the paste form
// (SignToo: the other party has signed already, A-340). TxID and RawTx are set once it was broadcast (P1-c2).
type EscrowSettlementView struct {
	ID                                    int64
	Title, Status, NetworkFee, InputTotal string
	Outputs                               []SettlementOutputView
	CanSign, SignToo, Stray               bool
	PartiesOnly                           bool   // a physical order's release before its outcome: both parties sign it, the market never does (escrow_release.go)
	State                                 string // the settlements.state
	PSBT, TxID, RawTx                     string
	Bumps                                 []*EscrowBumpView // its fee bumps, newest first (escrow_bump.go, P2-d)
	CanBump                               bool              // the claimant may ask to raise the fee of the broadcast transaction
	BumpRate, BumpMin, BumpMax            string            // the highest stored fee rate (sat/vB), and the lowest and highest a new bump may ask (A-383)
	Stalled                               bool              // staff only: broadcast, and its stall alert is open (A-441, escrow_stall.go)
}

// EscrowBumpView is one fee bump of a settlement on the order page (escrow_bump.go, P2-d): its state in honest words,
// its outputs while it is open (only the claimant's differs from the settlement's) and, for the party it waits for,
// that party's copy and the paste form.
type EscrowBumpView struct {
	ID, SettlementID    int64
	N                   int
	FeeRate, NetworkFee string
	Status              string
	Outputs             []SettlementOutputView
	CanSign, SignToo    bool
	PSBT                string
}

// EscrowFeeView is the Bitcoin escrow market fee in force on /admin (escrow_fee.go): basis points, the same as a
// percentage, the fee address ("" when the fee is 0); Available: escrow passed its checks, so the form can check an
// address with the node.
type EscrowFeeView struct {
	Bps, Percent, Address string
	Available             bool
	Descriptor            string // the fee descriptor's key fingerprint when the fee goes to a watch-only descriptor (A-460)
	HighestIndex          string // the highest fee index an escrow holds; "" when none
	Live                  bool   // a mainnet market: only a descriptor is accepted
}

// EscrowChainMismatch is the escrow chain pin (escrow_btc_chain) and the escrow node's chain identity startEscrow
// refused against it (A-439); Pinned is "" when the database holds escrows and no pin.
type EscrowChainMismatch struct {
	Pinned, Reported string
}

// EscrowNodeView is the line under Payment providers on /admin about the Bitcoin escrow node (escrow_watch.go): the
// reason class it is not synced for and the remedy when the reason does not say it (syncRemedy), the chain it was
// refused for; and (A-441, escrow_stall.go) why the Tor-mode gate holds broadcasts and the settlements queued or stuck.
type EscrowNodeView struct {
	Configured  bool
	NotSynced   string
	NotReady    string // Monero escrow only: the state its startup checks left it in while not ready (escrowXMR.notReady), with Remedy
	Remedy      string
	Mismatch    *EscrowChainMismatch
	ArbiterRead string // why the arbiter wallet's entries are not read (QA4W4A-1): "waiting" while the read goes on, or the failure's class
	ArbiterNext string // when the next read of them may start after a slow failure (A-479): "1 minute", "12 minutes"; "" = at the next poll
	TorHold     string
	Stalled     []EscrowStallView
	StallN      int // escrowStallBlocks
	Health      *EscrowHealthView
}

// EscrowHealthView is the persisted, allowlisted Bitcoin escrow node summary shown under Payment providers (A-451).
type EscrowHealthView struct {
	Enabled                                       bool
	DisabledClass, LastPass, StatusUpdated, Chain string
	Height, PinMatch                              string
	KeypoolHighWater                              string
	HighestMarketIndex                            int64
}

// EscrowStallView is a broadcast settlement whose transaction the watch-only wallet has not seen (escrow_stall.go): the
// order's ID (the order page opens for staff while the alert is open, A-470) and short ID, what the settlement pays and
// its transaction ID.
type EscrowStallView struct {
	ID, Order, What, TxID string
}

// EscrowAlertView is an escrow order with an open staff alert on the moderation desk: its ID, its state's label, and
// which alerts are open (a settlement queued or stuck, a release too few, a Monero deposit refused as another order's
// escrow account with no disposition recorded, A-480, or a requested Monero settlement older than 24 hours). A-560: the
// newest failed Monero settlement with a stored transaction (ReRequestable), without one claiming no deposit (Unbuilt,
// dismissed as nothing owed) and without one still claiming deposits (Stranded, PAY613-4: never dismissed), and the
// newest set-aside one (SetAside), by settlement ID, 0 for none; StoredText: what staff are told of the first
// (xmrFailedStoredText: whether a re-request waits, was refused or can be asked). Flagged (order page only): a deposit
// is flagged as well.
type EscrowAlertView struct {
	ID, State                                  string
	Stall, TooFew, Refused, Requested          bool
	ReRequestable, Unbuilt, Stranded, SetAside int64
	StoredText                                 string
	Flagged                                    bool
}

// XMRCollisionView is one record of a Monero escrow deposit refused as another order's escrow account (A-480), as staff
// read it on the order page: TxID (the first 20 characters only when Truncated, a record backfilled from a note written
// before migration 090), the wallet and account, when it was noted, and its disposition (Disposed, By, Note) once
// recorded. Form: the viewer may record the disposition now (open, and the viewer is staff who is not a party).
type XMRCollisionView struct {
	ID              int64
	TxID, Wallet    string
	Account         int64
	Truncated       bool
	Noted, Disposed string
	By, Note        string
	Form            bool
}

// EscrowSignerView is the line under Payment providers on /admin about the escrow signer (escrow_signer.go): State is
// signerWaiting, signerMismatch or signerBadReply.
type EscrowSignerView struct {
	State string
}

// EscrowReleaseDaysView is the Bitcoin escrow release deadline length in force on /admin (escrow_release_admin.go):
// Days, and the bounds the form accepts.
type EscrowReleaseDaysView struct {
	Days     string
	Min, Max int
}

// XMRFeeView is the Monero escrow market fee in force on Admin → Payments (escrow_xmr_fee.go): On while ESCROW_XMR is
// on; Bps its basis points; Percent and the full Address only above 0; Network the escrow's network in short words.
// It has no form: the fee is set on the server only (A-550).
type XMRFeeView struct {
	On                             bool
	Bps, Percent, Address, Network string
}

// EscrowDecisionView is a resolved escrow order's dispute decision (escrow_decision.go): Outcome release or refund,
// Text the honest sentence, CanChange for an administrator who is not a party while its settlement has no final
// transaction.
type EscrowDecisionView struct {
	Outcome, Text string
	CanChange     bool
}

// SettlementOutputView is one expected output of a settlement: the address, the amount and whom it pays.
type SettlementOutputView struct {
	Address, Amount, Role string
}

// EscrowCheckoutView offers escrow checkout on a draft (escrow_create.go): Reason says why it cannot be used now
// ("" = the form is offered); KeyLink adds a link to the buyer's escrow key page.
type EscrowCheckoutView struct {
	Reason   string
	KeyLink  bool
	Physical bool // the listing is shipped: the offer says the order stays open after funding (WR-escrow Phase 2)
}

// XMREscrowOrderView is a Monero escrow order's escrow on its page (escrow_xmr_order.go, WR-escrow Phase 3): amounts in
// XMR; RefundAddress set for the buyer only; Custody the custody sentence (A-402).
type XMREscrowOrderView struct {
	Available, RestorePaused                    bool
	Custody, Address, RefundAddress             string
	Required, Confirmed, Unconfirmed, Remaining string
	Threshold                                   int64
	Open, NeedsTopUp, ConfirmationWait          bool
	CancelAfter, Status, NextStep, Fee          string
	Deposits                                    []PaymentDeposit
	Unread                                      string // set when nothing is read at the address: shown for the Confirmed and Unconfirmed figures
	// Slice X-e (xmrReleaseView): a shipped order's release deadline, and the forms the viewer may use: the vendor's
	// Mark shipped and claim, the buyer's Confirm receipt.
	// Off: ESCROW_XMR is off, so none of those forms is offered.
	Deadline                  *EscrowDeadlineView
	Ship, Confirm, Claim, Off bool
	// Settlements: its settlements, newest first (escrow_xmr_settle_view.go)
	Settlements []*XMRSettlementView
	// Unrecoverable: set once the wallet that held its account was destroyed (ES-8, escrow_xmr_retain.go)
	Unrecoverable string
	// Net: the network of its escrow address, "fakechain", "stagenet" or "mainnet" (A-449, A-460)
	Net string
	// WindowPassed: Open, CancelAfter has passed and no deposit waits for confirmations: the watcher has not acted yet
	// (its daemon not synced or not readable; A-449, as EscrowOrderView's QA4W1-5)
	WindowPassed bool
}

// XMREscrowCheckoutView offers Monero escrow checkout on an XMR draft (escrow_xmr_create.go): Reason says why it cannot
// be used now; Net is Monero escrow's network and Prefixes the first characters of its addresses (A-449).
type XMREscrowCheckoutView struct {
	Reason, Custody string
	Physical        bool
	Net, Prefixes   string
	NetKnown        bool // the network was verified (App.xmrEscrowNetKnown): the copy names none before that (QA5W1-3)
}

// EscrowKeyView is the viewer's Bitcoin escrow key (escrow_account.go). Origin is the viewer's own key origin
// ("[fingerprint/path]"), shown only to them. Practice is the practice PSBT (base64), set only on request while
// PracticeOffered.
type EscrowKeyView struct {
	Available       bool // ESCROW_BTC=on passed its startup checks
	Registered      bool
	Origin          string
	ActiveAt        string // "" = inactive
	HasEscrows      bool
	Unreadable      bool // the stored key cannot be opened: APP_SECRET_KEY (or a legacy SETUP_TOKEN) changed
	PerOrder        bool // layout per_order: a per-order wallet key (A-455); otherwise a Bitcoin Core key
	PracticeOffered bool
	Practice        string
	PracticeError   string
	// A per-order wallet key's practice (A-455): the owner's key origin for the practice wallet, the two cosigner keys
	// (rooted Vpubs) and the practice wallet's first receiving address, set with Practice.
	PracticePath       string
	PracticeDerivation string
	PracticeCosigners  []string
	PracticeAddress    string
	NextChild          int64 // the child index (/0/<n>) the account's next escrow uses (escrowNextChildSQL)
	RangeAdvice        bool  // NextChild passed escrowRangeAdvice: the page advises keypoolrefill (P2-R10)
	KeypoolSize        int64 // the keypoolrefill size it advises: 1000 keys past NextChild
}
