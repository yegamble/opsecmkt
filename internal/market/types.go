package market

// Shared view types. Each feature package appends fields only inside its own commented block.

type User struct{ ID, Handle, Role, PGP, XMPP string }
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
type Dispute struct{ ID, OrderID, Reason, Status, Resolution, Created string }
type Event struct{ Action, Created string }

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

	// P2 PGP
	PGP *PGPView

	// P3 Orders
	OrderEvents   []OrderEvent
	Transitions   []Transition
	Delivery      *DeliveryView
	Reviews       []Review
	ReviewSummary *ReviewSummary
	CanReview     bool
	// IncomingOrders: vendor-dashboard orders on the viewer's own listings; NeedsAction counts paid ones.
	IncomingOrders []Order
	NeedsAction    int
	// DisputeOrders: order summary per dispute (disputes and moderator pages), keyed by order id.
	DisputeOrders map[string]Order

	// P4 Inventory (uses Product.Archived)
	Listing   *ListingView
	Inventory *InventoryView

	// P5 Payments
	Payment         *PaymentView
	Providers       []ProviderStatus
	Payout          *PayoutView
	PaymentsEnabled bool
	PaymentNetworks string
	Payouts         []PayoutRow // admin: recent payouts, newest first

	// P6 Transparency
	Canary      *CanaryView
	AuditExport *AuditExportView

	// Contacts (contacts.go): counterparty PGP keys, message recipient prefill, unread notifications
	Contacts            []ContactKey // vendor page: the vendor; order page: the other party (both for a reviewer); messages: the ?to recipient
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
	HasKey                                    bool   // a public key is saved on the profile
	KeyError                                  string // the saved key no longer parses (legacy or revoked)
	ChallengeExpires                          string // open ownership challenge expiry (UTC)
}

// P3 Orders
type OrderEvent struct{ From, To, Actor, Note, Created string }
type Transition struct{ To, Label, Action string } // Action "" = shown as unavailable with Label as the reason
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
	DeliveryContent     string // stored unencrypted; never loaded for public pages
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
	Attention                                        bool
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
	VerifiedAt           string
	Unreadable           bool // a key is saved but no longer parses (legacy or revoked)
}
