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

	// P4 Inventory (uses Product.Archived)

	// P5 Payments
	Payment         *PaymentView
	Providers       []ProviderStatus
	Payout          *PayoutView
	PaymentsEnabled bool
	PaymentNetworks string

	// P6 Transparency
	Canary      *CanaryView
	AuditExport *AuditExportView
}

// Foundation
type AuthView struct{ TOTPEnrolled, PGPEnrolled bool }

// P1 Authentication
type SecurityView struct {
	TOTPEnabled               bool
	PendingSecret, OTPAuthURI string
	RecoveryCodes             []string
}
type CaptchaView struct{ ID string }

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
type Transition struct{ To, Label, Action string }
type DeliveryView struct{ Content, Created string }
type Review struct {
	ID, OrderID, Buyer string
	Rating             int
	Body, Created      string
}
type ReviewSummary struct {
	Count   int
	Average string
}

// P5 Payments
type PaymentView struct {
	Network, Address                string
	Testnet                         bool
	Required, Received, Unconfirmed string
	Confirmations, Threshold        int
	Status                          string
}
type ProviderStatus struct {
	Currency, Network string
	Enabled           bool
	Error             string
}
type PayoutView struct{ BTC, XMR string }

// P6 Transparency
type CanaryView struct {
	Statement, Fingerprint, SignedAt, Posted, Error string
	Verified                                        bool
}
type AuditExportView struct {
	Available bool
	PublicKey string
	UpTo      int64
}
