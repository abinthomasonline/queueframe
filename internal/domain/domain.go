package domain

import "time"

const (
	OrderStateScheduled      = "SCHEDULED"
	OrderStateDraft          = "DRAFT"
	OrderStateArmed          = "ARMED"
	OrderStatePrepared       = "PREPARED"
	OrderStateDispatching    = "DISPATCHING"
	OrderStateAcknowledged   = "ACKNOWLEDGED"
	OrderStateComplete       = "COMPLETE"
	OrderStateRejected       = "REJECTED"
	OrderStateCancelled      = "CANCELLED"
	OrderStateUnknown        = "UNKNOWN"
	OrderStateMissed         = "MISSED"
	OrderStateMissedInactive = "MISSED_INACTIVE"
	OrderStateDeleted        = "DELETED"
)

const (
	KiteAuthSignup = "SIGNUP"
	KiteAuthSignin = "SIGNIN"
)

const (
	HostingPolicyVersion   = "exclusive-household-static-ip-v1"
	HostingPolicySourceURL = "https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip"
	HostingPolicyExactText = "I confirm that this application instance is hosted exclusively for my use and/or the use of my immediate family members permitted by Zerodha (my spouse, dependent children, and dependent parents), and that its displayed static egress IP is not shared with or used for API order placement by anyone outside this group."
)

type User struct {
	ID                 int64
	Username           string
	Admin              bool
	MustChangePassword bool
	CreatedAt          time.Time
}

// Admin is a local operator identity. It is deliberately separate from a
// Zerodha trader identity: traders authenticate through Kite, while admins
// authenticate with a local password. The root admin is protected from
// deletion and demotion.
type Admin struct {
	ID                 int64
	Username           string
	Root               bool
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// PolicyAttestation is the exact declaration accepted during onboarding.
// Keeping the version, source, and complete text makes an acknowledgement
// auditable even when the published policy changes later.
type PolicyAttestation struct {
	Version        string
	SourceURL      string
	Text           string
	AcknowledgedAt time.Time
}

// Trader is one Zerodha client ID and its single Kite Connect application.
// Secret fields are populated only when an authorized caller explicitly loads
// credentials from the encrypted store.
type Trader struct {
	ID                int64
	ClientID          string
	DisplayClientID   string
	APIKey            string
	APISecret         string
	AccessToken       string
	VerifiedAt        time.Time
	SessionLoginAt    time.Time
	SessionExpiresAt  time.Time
	ActivatedForDate  string
	PolicyAttestation PolicyAttestation
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         time.Time
}

func (t Trader) Deleted() bool { return !t.DeletedAt.IsZero() }

// DailyActivation is credential-free metadata for one trader's date-bound
// Kite session. TokenPresent records that encrypted token material exists
// without exposing it to web or administrator list views.
type DailyActivation struct {
	TraderID      int64
	ExecutionDate string
	LoginAt       time.Time
	ExpiresAt     time.Time
	TokenPresent  bool
}

// ActiveFor reports whether this date-bound session can cover target. The
// scheduler performs the same checks on its credential-bearing Trader value;
// web and administrator views use this metadata form instead.
func (a DailyActivation) ActiveFor(executionDate string, target time.Time) bool {
	return a.TraderID > 0 && a.ExecutionDate == executionDate && a.TokenPresent &&
		a.LoginAt.Before(target) && target.Before(a.ExpiresAt)
}

// ActiveFor reports whether the trader completed a post-06:00 Kite login for
// the requested IST trading date and the resulting token survives the target.
func (t Trader) ActiveFor(executionDate string, target time.Time) bool {
	return !t.Deleted() && t.ActivatedForDate == executionDate && t.AccessToken != "" &&
		t.SessionLoginAt.Before(target) && target.Before(t.SessionExpiresAt)
}

// KiteAuthFlow is a short-lived, single-use login handoff. State is returned
// only when a flow is created; persisted state is SHA-256 hashed.
type KiteAuthFlow struct {
	ID                 int64
	State              string
	Purpose            string
	TraderID           int64
	SignupInvitationID int64
	ClientID           string
	APIKey             string
	APISecret          string
	PolicyAttestation  PolicyAttestation
	ExpiresAt          time.Time
	ConsumedAt         time.Time
	CompletedAt        time.Time
}

type TraderSignup struct {
	ClientID  string
	APIKey    string
	APISecret string
	// InviteToken is the high-entropy bearer supplied by the instance
	// administrator. It is hashed before lookup and is never persisted.
	InviteToken       string
	PolicyAttestation PolicyAttestation
}

// SignupInvitation grants one household member permission to begin one
// first-time Kite onboarding flow. Raw tokens are deliberately absent from
// this model: CreateSignupInvitation returns the bearer separately exactly
// once, while every stored and listed representation contains metadata only.
type SignupInvitation struct {
	ID                 int64
	CreatedByAdminID   int64
	CreatedByUsername  string
	ExpiresAt          time.Time
	ReservedAt         time.Time
	ReservedUntil      time.Time
	ConsumedAt         time.Time
	ConsumedByTraderID int64
	ConsumedByClientID string
	RevokedAt          time.Time
	RevokedByAdminID   int64
	RevokedByUsername  string
	CreatedAt          time.Time
}

// RuntimeSettings is versioned and becomes effective on a trading date.
// Activation remains a fixed 06:00 IST safety rule and is intentionally not
// configurable.
type RuntimeSettings struct {
	ID                 int64
	Version            int64
	EffectiveDate      string
	DispatchClock      string
	UserBlackoutStart  string
	UserBlackoutEnd    string
	FreezeLead         time.Duration
	BatchInterval      time.Duration
	MaxOrdersPerTrader int
	CreatedByAdminID   int64
	CreatedAt          time.Time
}

// Account is one Zerodha trading account connected to this household instance.
// Secrets are decrypted only while the process is running.
type Account struct {
	ID               int64
	OwnerUserID      int64
	Name             string
	ClientID         string
	APIKey           string
	APISecret        string
	AccessToken      string
	SessionLoginAt   time.Time
	SessionExpiresAt time.Time
	Verified         bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (a Account) SessionReady(now time.Time) bool {
	return a.Verified && a.AccessToken != "" && now.Before(a.SessionExpiresAt)
}

// OrderParams is the immutable, user-supplied regular-order payload. Price and
// percentage values stay as decimal strings until the Kite SDK boundary.
type OrderParams struct {
	Exchange          string
	TradingSymbol     string
	TransactionType   string
	Product           string
	OrderType         string
	Quantity          int
	DisclosedQuantity int
	Price             string
	TriggerPrice      string
	Validity          string
	ValidityTTL       int
	MarketProtection  string
	Autoslice         bool
}

type OrderIntent struct {
	ID            int64
	OwnerUserID   int64
	AccountID     int64
	ExecutionDate string // YYYY-MM-DD in Asia/Kolkata.
	Priority      int
	Tag           string
	State         string
	Params        OrderParams
	LiveConfirmed bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ArmedAt       time.Time
}

// QueuedOrder is the v2 trader-owned order model. Position is dense within a
// trader/date queue and replaces a user-entered priority. The broker payload
// always uses DAY validity and internal market protection.
type QueuedOrder struct {
	ID            int64
	TraderID      int64
	ExecutionDate string
	Position      int
	Tag           string
	State         string
	Params        OrderParams
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     time.Time
}

type OrderQueue struct {
	TraderID      int64
	ExecutionDate string
	Revision      int64
	FrozenAt      time.Time
	Orders        []QueuedOrder
}

type QueueCandidate struct {
	Order  QueuedOrder
	Trader Trader
}

type DispatchResult struct {
	IntentID                int64
	AccountID               int64
	Tag                     string
	Attempt                 int
	RequestCount            int
	TargetAt                time.Time
	StartedAt               time.Time
	FinishedAt              time.Time
	OrderTimestamp          time.Time
	ExchangeTimestamp       time.Time
	ExchangeUpdateTimestamp time.Time
	Outcome                 string
	OrderID                 string
	ExchangeOrderID         string
	BrokerStatus            string
	ChildOrders             string
	ErrorType               string
	ErrorMessage            string
	// Receipts are append-only observations collected after placement has
	// returned. They are deliberately carried with the in-memory result so the
	// scheduler can persist them outside the latency-sensitive order path.
	Receipts []DispatchReceipt
}

const (
	DispatchReceiptKiteResponse    = "KITE_RESPONSE"
	DispatchReceiptOrderBook       = "ORDERBOOK"
	DispatchReceiptCurrentSnapshot = "CURRENT_SNAPSHOT"
)

// DispatchReceipt is a point-in-time broker acknowledgement. ObservedAt is
// Queueframe's local receipt time; the three broker timestamps retain the
// separate OMS and exchange clocks returned by Kite.
type DispatchReceipt struct {
	ID                      int64
	Source                  string
	ObservedAt              time.Time
	OrderID                 string
	ExchangeOrderID         string
	BrokerStatus            string
	StatusMessage           string
	OrderTimestamp          time.Time
	ExchangeTimestamp       time.Time
	ExchangeUpdateTimestamp time.Time
}

// DispatchRecord is the durable, user-facing history for a queued order. A
// record is written before the hot path, and RequestCount is constrained to
// zero or one so no persistence path can describe an automatic resubmission.
type DispatchRecord struct {
	ID            int64
	QueuedOrderID int64
	TraderID      int64
	ClientID      string
	ExecutionDate string
	Position      int
	Batch         int
	PayloadJSON   string
	Result        DispatchResult
	Receipts      []DispatchReceipt
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Instrument struct {
	Exchange       string
	TradingSymbol  string
	Name           string
	Segment        string
	InstrumentType string
	Expiry         time.Time
}
