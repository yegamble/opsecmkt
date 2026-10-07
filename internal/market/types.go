package market

import "html/template"

// Shared view types. Each feature package appends fields only inside its own commented block.

type User struct {
	ID, Handle, Role, PGP, XMPP string
	Factors                     string // P1, PageData.FactorAccounts only: enrolled second factors, e.g. "TOTP and PGP sign-in"
	Suspended                   string // P1, PageData.SuspendedAccounts only: when the account was suspended
	Avatar                      string // A-189, vendor page and /canary staff list only: the avatar thumbnail on /img, "" when none may be shown
}
type Product struct {
	ID, Title, Description, Category, Region, Kind, Vendor, VendorID, PriceBTC, PriceXMR string
	Stock                                                                                int
	Archived                                                                             bool // Foundation; P4 uses
	// P4, vendor-dashboard only: an administrator's takedown reason (A-163); never loaded for public pages.
	TakedownReason string
	// A-189, catalog, vendor and product pages only: the listing's public photos, oldest first (image_pages.go),
	// and Thumb, the first one's thumbnail URL for its card ("" = the CSS illustration).
	Photos []ProductPhoto
	Thumb  string
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
	// Escrow: settled through a per-order escrow instead of the pooled wallet (settleTerminal, escrowOnly): the order
	// has an escrows row (WR-escrow Phase 1, escrow_create.go), of either coin (btcEscrow).
	Escrow bool
}
type Message struct {
	ID, Sender, Recipient, Body, Created string
	// P2 PGP
	Encrypted      bool
	RecipientMatch string
}
type Notification struct {
	ID, Body, Created string
	Read              bool
}

// StaffContactView is the administrator's staff contact (A-119, staff_contact.go): Text as saved, shown escaped;
// Handle set only when a signed-in reader may be linked to /messages?to=<Handle>.
type StaffContactView struct{ Text, Handle string }
type Dispute struct {
	ID, OrderID, Reason, Status, Resolution, Created string
	NoResolver                                       bool // open, and every moderator and administrator is a party
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

type PageData struct {
	Page, Title, CSRF, Error, Notice, Query, Category, Region, Currency, Mode string
	NoticeAt                                                                  string // the section a one-shot notice renders in, "" = the top (notice.go)
	User                                                                      *User
	Products                                                                  []Product
	Product                                                                   *Product
	Orders                                                                    []Order
	Order                                                                     *Order
	Messages                                                                  []Message
	Notifications                                                             []Notification
	Disputes                                                                  []Dispute
	Users                                                                     []User
	Events                                                                    []Event
	Settings                                                                  map[string]string
	Preview                                                                   bool
	// Paging: catalog and vendor page, the page shown of the matching listings (A-181, A-189, load.go).
	Paging *CatalogPaging
	// UploadBack and UploadBackLabel: the refusal page of an upload (A-186, uploads.go) links back here.
	UploadBack, UploadBackLabel string
	// ImageStorage: admin page, stored photos and avatars against IMAGE_STORAGE_LIMIT_MB (uploads.go).
	ImageStorage *ImageStorageView
	// A-189 (image_pages.go). StaffListPublic: /canary, the administrator's "Show a public staff list" is on;
	// StaffList: then the moderators and administrators listed (User.Avatar set when it may be shown).
	StaffListPublic bool
	StaffList       []User
	// StaffContact: /canary, /challenge, /login, /register, /notifications, /vendor-dashboard and /listing-edit (a
	// taken-down listing, A-279), the administrator's staff contact (A-119, staff_contact.go); nil while unset.
	StaffContact *StaffContactView
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
	// RoleHandle, RoleChoice, ResetHandle, SuspendHandle and SuspendChoice: admin page re-rendered after a
	// handle matched no account, keeping what was typed into the role, second-factor reset or suspension form.
	RoleHandle, RoleChoice, ResetHandle, SuspendHandle, SuspendChoice string
	// ResolveDispute and ResolveText: the desk re-rendered after a resolve form loaded before the other party's
	// reply was refused (A-280), keeping the decision typed for that dispute.
	ResolveDispute, ResolveText string

	// P2 PGP
	PGP *PGPView

	// P3 Orders
	OrderEvents   []OrderEvent
	Transitions   []Transition
	Delivery      *DeliveryView
	Reviews       []Review
	ReviewSummary *ReviewSummary
	CanReview     bool
	// IncomingOrders: vendor-dashboard orders on the viewer's own listings, every paid one first;
	// NeedsAction counts all paid ones.
	IncomingOrders []Order
	NeedsAction    int
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
	// Overpaid: order page for its buyer or vendor, a paid, shipped or delivered order whose counted deposits
	// exceed the price; nil otherwise.
	Overpaid *OverpaidView
	// HistoryLimit: non-zero when closed history (resolved disputes, or incoming orders not paid) was
	// cut to this many most recent rows.
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

	// P4 Inventory (uses Product.Archived)
	Listing   *ListingView
	Inventory *InventoryView

	// P5 Payments
	Payment         *PaymentView
	Providers       []ProviderStatus
	Payout          *PayoutView
	PaymentsEnabled bool
	PaymentNetworks string
	LiveNetwork     bool        // payment providers are on mainnet: amounts are real funds
	Payouts         []PayoutRow // admin: every payout needing attention (oldest first), then recent others (newest first)
	// PayoutsAttention counts the leading Payouts that need attention; PayoutHistoryLimit is non-zero when
	// the other payouts were cut to this many most recent rows.
	PayoutsAttention   int
	PayoutHistoryLimit int
	// MyPayout: order page, the viewer's own payout on this order while it is not sent (A-120).
	// UnsentPayouts: vendor desk, how many payouts to the viewer are not sent (listed on /account#payout).
	MyPayout      *RecipientPayout
	UnsentPayouts int
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
	// coins, counted like EscrowOrders. XMREscrowNet: Monero escrow's network, "fakechain" or "stagenet"
	// (App.xmrEscrowNetwork, A-449), so the notices name it. XMREscrowNetKnown: that network was verified (escrowXMR.formNet
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
	// EscrowSigner: /admin while ESCROW_SIGNER_SOCKET is set and the escrow signer is not ready (A-454)
	EscrowSigner *EscrowSignerView
	// EscrowReleaseDays: /admin while ESCROW_BTC is on, the release deadline length in force and its form (escrow_release_admin.go)
	EscrowReleaseDays *EscrowReleaseDaysView
	// EscrowRecovery: /admin, the Bitcoin escrow restore gate, held settlements and orphan addresses (escrow_recovery.go)
	EscrowRecovery *EscrowRecoveryView
	// XMREscrowRecovery: /admin, Monero escrow settlements held by a restore and destroyed wallets' files
	// (escrow_xmr_restore.go)
	XMREscrowRecovery *XMREscrowRecoveryView
	// Escrow: order page of an escrow order, its escrow (escrow_order.go); EscrowCheckout: order page of the buyer's
	// BTC draft on a listing that takes escrow, while ESCROW_BTC is on. Both nil otherwise.
	Escrow         *EscrowOrderView
	EscrowCheckout *EscrowCheckoutView
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
	Contacts            []ContactKey // vendor page: the vendor; order page: the other party (both for a reviewer); messages: the ?to recipient
	StaffContacts       []ContactKey // disputed order: independent moderators and administrators
	MessageTo           string       // messages page: validated ?to handle for the recipient field
	OrderViewer         string       // order page: "buyer", "vendor" or "moderator" (read-only dispute review)
	UnreadNotifications int          // signed-in users, every page
	// VendorKeyUnusable (A-83): a physical listing's product and checkout pages, and its buyer's order page, when the
	// vendor has no key that can receive encrypted messages (canReceiveEncrypted), so no shipping address can be sent.
	VendorKeyUnusable bool
	// PurchaseBlocks (A-444): a listing's product and checkout pages, each coin whose intake is escrow only while the
	// listing cannot use its escrow (escrow_readiness.go), PurchaseBlocked the same by coin, and NoPurchase when every
	// coin is blocked: the listing is not available for purchase.
	PurchaseBlocks  []PurchaseBlock
	PurchaseBlocked map[string]bool
	NoPurchase      bool
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
	Payout            *PayoutPreview // complete, and cancel of a paid order: the payout it queues
}

// PayoutPreview states what a form that queues a payout (complete, a cancel of a paid order or of an order
// awaiting payment that has received a deposit, resolve)
// pays now, by payoutBasis (A-161, A-122). Seen is that amount in atomic units, sent back as amount_seen;
// Confirm labels the required confirmation checkbox. Resolve form only: Release and Refund state each outcome
// (amount, recipient, difference from the price, the recipient's payout state); Escrow, when set, how a Monero escrow
// order's decision is paid (xmrDisputePreview).
type PayoutPreview struct {
	Seen                             int64
	Confirm, Release, Refund, Escrow string
}

// OverpaidView says what completing an overpaid order does with the excess, from the split the complete form
// states (splitPayout, A-215). Excess: "0.009 BTC". Kept: it goes to the vendor with the release, for Reason
// (below the minimum automatic payout, or a legacy price below the release floor, A-216). Held: larger than the
// price, so an administrator reviews its return (or, when Kept, the release) first. NoAddress: buyer's view of a
// return only, no payout address saved in the order's currency.
type OverpaidView struct {
	Excess, Reason        string
	Kept, Held, NoAddress bool
}
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
	OpenOrders          int    // orders not completed, resolved or cancelled
	AutoDelivery        string // currencies with a configured payment provider, e.g. "BTC, XMR"; "" = none
	TakedownReason      string // an administrator's takedown reason (A-163); "" when not taken down
	DispatchDays        int    // a physical listing's dispatch window in days (P2-R5); the default shown when none is set
}
type InventoryView struct {
	Active, Archived int
	AutoDelivery     string
}

// P5 Payments
type PaymentView struct {
	Network, Address                string
	Testnet                         bool
	Required, Received, Unconfirmed string
	Remaining                       string
	NeedsTopUp                      bool
	Confirmations, Threshold        int
	Status                          string
	Currency                        string
	Available                       bool // a live provider serves this order's currency
	Degraded                        bool // the currency is configured but its wallet is failing its checks
	Issued                          bool // the provider returned a deposit address for this order
	Monitored                       bool // Issued and the provider is still configured
	Open                            bool // Monitored and the order still awaits payment (address shown)
	Deposits                        []PaymentDeposit
	Payouts                         []PayoutRow // release and return of any excess, or refund (ORDER BY id)
	// PaymentURI and QR: while the order is open and something is still due, the wallet payment request for
	// exactly that amount (paymentURI) and its inline SVG QR code (qrSVG); empty otherwise.
	PaymentURI string
	QR         template.HTML
	// CancelAfter: while the order is open, the time ("YYYY-MM-DD HH:MM UTC") from which the watcher can cancel
	// it if still unpaid or short (A-123); RefundFloor: the minimum automatic payout in this currency.
	CancelAfter, RefundFloor string
}
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

// ReadinessCoin: Intake is payment_intake_<COIN>; Pooled, the coin's pooled wallet is configured; NotReady, why the
// coin's escrow, on, is not ready ("" when it is, QA4W4B-1).
type ReadinessCoin struct {
	Currency, Intake, NotReady string
	EscrowOn, Pooled           bool
}
type ReadinessListing struct{ ID, Title, Vendor, Kind, Reason string }
type ReadinessVendor struct {
	Handle   string
	Listings int
}

type ProviderStatus struct {
	Currency, Network string
	Enabled           bool
	Configured        bool   // the pooled wallet is configured in this process, available or not (pooledConfigured)
	Status            string // Enabled, Unavailable (retried), Refused, Disabled
	Error             string
	Confirmations     int
	LastPoll          string
	Version           string // wallet or node version its last check read (versionReporter)
	VersionWarning    string // set when send errors are all ambiguous because Version is not the verified one
	// AddressCollisions: payment requests refused because the wallet returned an address already issued (A-288),
	// since an administrator last dismissed the warning; AddressCollisionLast: when the latest one was refused.
	AddressCollisions    int
	AddressCollisionLast string
}
type PayoutView struct {
	BTC, XMR               string
	BTCNetwork, XMRNetwork string // empty = no provider for that currency
	Blocked                int    // payouts waiting for this user's address
	Unsent                 []RecipientPayout
}

// RecipientPayout is a payout as its recipient sees it (A-120): every payout to the viewer that is not sent, on
// /account#payout (oldest first, uncapped) and on its order page. Status and NextStep are fixed copy chosen by
// recipientPayout from the payout's state and hold; the wallet's error text is never shown. Address is the
// payout's destination ("" while none is set), shown to its recipient only.
type RecipientPayout struct {
	OrderID, OrderShort, Kind, Currency, Amount, Network string
	Address, Status, NextStep                            string
}
type PayoutRow struct {
	ID                                               int64
	OrderID, Kind, Recipient, Currency, Amount       string
	Address, State, StateLabel, TxID, Error, Updated string
	OrderShort                                       string // shortID(OrderID), named in the resolve summary
	Attention                                        bool
	OrderLink                                        bool // the viewing administrator can open /order for it
	// Ambiguous: the payout may already have been broadcast (failed without a definite wallet answer, stuck
	// in sending, or held by a restore from backup); requeueing or releasing needs an explicit confirmation.
	Ambiguous bool
	// AddressCheck: held for an account suspension (suspendedHoldPrefix); releasing needs an explicit
	// confirmation that the payout address was checked.
	AddressCheck bool
	// LargeCheck: a refund or return larger than the order price (largeHoldPrefix); releasing needs an explicit
	// confirmation that an administrator reviewed it.
	LargeCheck bool
	// CurrentAddress: the recipient's saved payout address for Currency now; CurrentChanged: when it was last saved
	// ("" when not recorded). Repoint: the payout may be moved to CurrentAddress (repointable, A-121).
	CurrentAddress, CurrentChanged string
	Repoint                        bool
	// Waiting: held by the watcher (heldReason) while these credited deposits are below Threshold confirmations;
	// no release is offered until they confirm again.
	Waiting   []HeldDeposit
	Threshold int64
}

// HeldDeposit is a credited deposit, at its deposit address, that holds its order's payout.
type HeldDeposit struct {
	TxID, Address        string
	Index, Confirmations int64
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
	Open, NeedsTopUp                            bool
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
}

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
}

// EscrowChainMismatch is the escrow chain pin (escrow_btc_chain) and the escrow node's chain identity startEscrow
// refused against it (A-439); Pinned is "" when the database holds escrows and no pin.
type EscrowChainMismatch struct {
	Pinned, Reported string
}

// EscrowNodeView is the line under Payment providers on /admin about the Bitcoin escrow node (escrow_watch.go): the
// reason class it is not synced for and the remedy when the reason does not say it (syncRemedy), the chain it was
// refused for, or the class of the answer the pooled user got to the market key self-check (PooledKey, A-440) with the
// pooled rpcwhitelist methods it should be restricted to; and (A-441, escrow_stall.go) why the Tor-mode gate holds
// broadcasts and the settlements queued or stuck.
type EscrowNodeView struct {
	NotSynced       string
	NotReady        string // Monero escrow only: the state its startup checks left it in while not ready (escrowXMR.notReady), with Remedy
	Remedy          string
	Mismatch        *EscrowChainMismatch
	PooledKey       string
	PooledWhitelist string
	ArbiterRead     string // why the arbiter wallet's entries are not read (QA4W4A-1): "waiting" while the read goes on, or the failure's class
	ArbiterNext     string // when the next read of them may start after a slow failure (A-479): "1 minute", "12 minutes"; "" = at the next poll
	TorHold         string
	Stalled         []EscrowStallView
	StallN          int // escrowStallBlocks
}

// EscrowStallView is a broadcast settlement whose transaction the watch-only wallet has not seen (escrow_stall.go): the
// order's ID (the order page opens for staff while the alert is open, A-470) and short ID, what the settlement pays and
// its transaction ID.
type EscrowStallView struct {
	ID, Order, What, TxID string
}

// EscrowAlertView is an escrow order with an open staff alert on the moderation desk: its ID, its state's label, and
// which alerts are open (a settlement queued or stuck, a release too few).
type EscrowAlertView struct {
	ID, State     string
	Stall, TooFew bool
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
	Open, NeedsTopUp                            bool
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
	// Net: the network of its escrow address, "fakechain" or "stagenet" (A-449)
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
