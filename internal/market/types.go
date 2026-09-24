package market

// Shared view types. Each feature package appends fields only inside its own commented block.

type User struct {
	ID, Handle, Role, PGP, XMPP string
	Factors                     string // P1, PageData.FactorAccounts only: enrolled second factors, e.g. "TOTP and PGP sign-in"
	Suspended                   string // P1, PageData.SuspendedAccounts only: when the account was suspended
}
type Product struct {
	ID, Title, Description, Category, Region, Kind, Vendor, VendorID, PriceBTC, PriceXMR string
	Stock                                                                                int
	Archived                                                                             bool // Foundation; P4 uses
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

// PaymentReview is one deposit the payment watcher flagged for staff review (payments.flagged). Open: not yet
// settled by events (a locked transfer or a late deposit, which have no disposition action yet, or a credited
// deposit in an announced regression that has not confirmed again).
type PaymentReview struct {
	OrderID, OrderState, Currency, Amount, TxID, Reason, Flagged string
	Index                                                        int64
	Open                                                         bool
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

	// Foundation
	Auth *AuthView

	// P1 Authentication
	Security *SecurityView
	Captcha  *CaptchaView
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
	// exceed the price by this much ("0.009 BTC"); "" otherwise.
	Overpaid string
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
	Payouts         []PayoutRow // admin: every payout needing attention (oldest first), then recent others (newest first)
	// PayoutsAttention counts the leading Payouts that need attention; PayoutHistoryLimit is non-zero when
	// the other payouts were cut to this many most recent rows.
	PayoutsAttention   int
	PayoutHistoryLimit int

	// P6 Transparency
	Canary      *CanaryView
	AuditExport *AuditExportView

	// Contacts (contacts.go): counterparty PGP keys, message recipient prefill, unread notifications
	Contacts            []ContactKey // vendor page: the vendor; order page: the other party (both for a reviewer); messages: the ?to recipient
	StaffContacts       []ContactKey // disputed order: independent moderators and administrators
	MessageTo           string       // messages page: validated ?to handle for the recipient field
	OrderViewer         string       // order page: "buyer", "vendor" or "moderator" (read-only dispute review)
	UnreadNotifications int          // signed-in users, every page
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
}

// P3 Orders
type OrderEvent struct{ From, To, Actor, Note, Created string }
type Transition struct {
	To, Label, Action string         // Action "" = shown as unavailable with Label as the reason
	Payout            *PayoutPreview // complete, and cancel of a paid order: the payout it queues
}

// PayoutPreview states what a form that queues a payout (complete, a vendor's cancel of a paid order, resolve)
// pays now, by payoutBasis (A-161, A-122). Seen is that amount in atomic units, sent back as amount_seen;
// Confirm labels the required confirmation checkbox. Resolve form only: Release and Refund state each outcome
// (amount, recipient, difference from the price, the recipient's payout state).
type PayoutPreview struct {
	Seen                     int64
	Confirm, Release, Refund string
}
type DeliveryView struct{ Content, Created string }
type Review struct {
	ID, OrderID, Buyer string
	Rating             int
	Body, Created      string
	Product            string // listing title, vendor page only
}
type ReviewSummary struct {
	Count   int
	Average string
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
	Payout                          *PayoutRow
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
}
type PayoutView struct {
	BTC, XMR               string
	BTCNetwork, XMRNetwork string // empty = no provider for that currency
	Blocked                int    // payouts waiting for this user's address
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
}
