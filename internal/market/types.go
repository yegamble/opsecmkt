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
type Dispute struct {
	ID, OrderID, Reason, Status, Resolution, Created string
	NoResolver                                       bool // open, and every moderator and administrator is a party
}

// PaymentReview is one deposit the payment watcher flagged for staff review (payments.flagged) with no recorded
// disposition, or one escalated to an administrator (A-31). Open: a locked transfer, a late deposit or a deposit of
// a payout below the minimum, or a credited deposit in an announced regression that has not confirmed again.
// ID is payments.id. Party: the viewer is the order's buyer or vendor. Form: the viewer may record the disposition
// (open, not a party, and an administrator when Escalated). Refundable: not credited, so "refunded by hand" applies.
type PaymentReview struct {
	ID, OrderID, OrderState, Currency, Amount, TxID, Reason, Flagged string
	Index                                                            int64
	Open, Escalated, Party, Form, Refundable                         bool
}
type Event struct{ Handle, Action, Created string } // Handle: account the audit row is recorded on ("" = system)

type PageData struct {
	Page, Title, CSRF, Error, Notice, Query, Category, Region, Currency, Mode string
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
	// A-190 (image_takedown.go). RecentImages: /moderator, the newest public photos and avatars (vendors' only for a moderator); AvatarTakedown:
	// vendor page, the staff remove form for the vendor's avatar.
	RecentImages   []RecentImage
	AvatarTakedown *ImageTakedown

	// Foundation
	Auth *AuthView

	// P1 Authentication
	Security *SecurityView
	Captcha  *CaptchaView
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
	// DisputeOrders: order summary per dispute (disputes and moderator pages), keyed by order id.
	// OpenDisputes: how many leading entries of Disputes are open (the rest are resolved).
	DisputeOrders map[string]Order
	OpenDisputes  int
	// DisputePayouts: resolve form per open dispute the viewer may resolve, keyed by dispute id.
	DisputePayouts map[string]PayoutPreview
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
// (amount, recipient, difference from the price, the recipient's payout state).
type PayoutPreview struct {
	Seen                     int64
	Confirm, Release, Refund string
}

// OverpaidView says what completing an overpaid order does with the excess, from the split the complete form
// states (splitPayout, A-215). Excess: "0.009 BTC". Kept: it goes to the vendor with the release, for Reason
// (below the minimum automatic refund, or a legacy price below the release floor, A-216). Held: larger than the
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
	// it if still unpaid or short (A-123); RefundFloor: the minimum automatic refund in this currency.
	CancelAfter, RefundFloor string
}
type PaymentDeposit struct {
	TxID          string
	Index         int64
	Amount        string
	Confirmations int64
	State         string
}
type ProviderStatus struct {
	Currency, Network string
	Enabled           bool
	Status            string // Enabled, Unavailable (retried), Refused, Disabled
	Error             string
	Confirmations     int
	LastPoll          string
	Version           string // wallet or node version its last check read (versionReporter)
	VersionWarning    string // set when send errors are all ambiguous because Version is not the verified one
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
