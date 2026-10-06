// Copyright 2026 Elliott Wave LLC. All rights reserved.

package client

import (
	"time"

	"github.com/moqom-cloud/motom/events"
)

// TipEvent is what a room is told about a tip.
type TipEvent = events.Tip

// Posting moves Amount into (positive, a debit) or out of (negative, a
// credit) an account. An entry's postings sum to zero per currency.
type Posting struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

// Entry is an immutable ledger entry.
type Entry struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Postings  []Posting         `json:"postings"`
	Reverses  string            `json:"reverses,omitempty"`
	Reference string            `json:"reference,omitempty"`
	Memo      string            `json:"memo,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Wallet is a user's credit balances by currency.
type Wallet struct {
	User         string           `json:"user"`
	Frozen       bool             `json:"frozen"`
	FrozenReason string           `json:"frozen_reason,omitempty"`
	Balances     map[string]int64 `json:"balances"`
	CreatedAt    time.Time        `json:"created_at"`
}

// Pack is a credit pack: pay Price, receive Price+Bonus credits.
type Pack struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Price    int64  `json:"price"`
	Bonus    int64  `json:"bonus"`
}

// TenantConfig is a tenant's commercial settings. Amounts are minor units;
// fees are basis points (100 = 1%).
type TenantConfig struct {
	TenantFee           int    `json:"tenant_fee_bps"`
	Packs               []Pack `json:"packs"`
	TipMin              int64  `json:"tip_min"`
	TipMax              int64  `json:"tip_max"`
	SenderDailyCap      int64  `json:"sender_daily_cap"`
	RoomDailyCap        int64  `json:"room_daily_cap"`
	SenderTipsPerMin    int    `json:"sender_tips_per_minute"`
	RoomTipsPerMin      int    `json:"room_tips_per_minute"`
	PurchasesPerHour    int    `json:"purchases_per_hour"`
	ReviewTipAt         int64  `json:"review_tip_at"`
	RepeatPairPerHour   int    `json:"repeat_pair_per_hour"`
	HoldDays            int    `json:"hold_days"`
	MinPayout           int64  `json:"min_payout"`
	ReviewFirstPayoutAt int64  `json:"review_first_payout_at"`
	// ChargebackLiability is "platform" or "tenant": who absorbs credits
	// already spent when a purchase is charged back.
	ChargebackLiability string `json:"chargeback_liability"`
}

// Tenant is the calling key's tenant.
type Tenant struct {
	ID          string       `json:"id"`
	Currencies  []string     `json:"currencies"`
	PlatformFee int          `json:"platform_fee_bps"`
	Config      TenantConfig `json:"config"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Key is an API key without its secret.
type Key struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Purchase is a credit pack purchase. Status is pending, succeeded, failed,
// refunded, disputed or flagged.
type Purchase struct {
	ID           string    `json:"id"`
	User         string    `json:"user"`
	Pack         string    `json:"pack"`
	Currency     string    `json:"currency"`
	Price        int64     `json:"price"`
	Credits      int64     `json:"credits"`
	Refunded     int64     `json:"refunded"`
	Status       string    `json:"status"`
	Mode         string    `json:"mode"`
	CheckoutURL  string    `json:"checkout_url,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Tip is a recorded tip.
type Tip struct {
	ID        string    `json:"id"`
	Room      string    `json:"room"`
	Sender    string    `json:"sender"`
	Creator   string    `json:"creator"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Message   string    `json:"message,omitempty"`
	EntryID   string    `json:"entry_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Earnings is a creator's money in one currency. Held is earned within the
// hold period; Available is what the next payout run can pay; Pending is in
// flight to Stripe.
type Earnings struct {
	Currency  string `json:"currency"`
	Balance   int64  `json:"balance"`
	Held      int64  `json:"held"`
	Available int64  `json:"available"`
	Pending   int64  `json:"pending"`
}

// Creator is a creator's payout status. KYC is none, pending, verified or
// restricted.
type Creator struct {
	Creator        string     `json:"creator"`
	StripeAccount  string     `json:"stripe_account,omitempty"`
	KYC            string     `json:"kyc"`
	PayoutsEnabled bool       `json:"payouts_enabled"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	Frozen         bool       `json:"frozen"`
	Payable        bool       `json:"payable"`
	OpenReviews    int        `json:"open_reviews"`
	Earnings       []Earnings `json:"earnings"`
}

// Payout is a transfer to a creator's connected account. Status is pending,
// paid, failed or returned.
type Payout struct {
	ID             string    `json:"id"`
	Creator        string    `json:"creator"`
	Currency       string    `json:"currency"`
	Amount         int64     `json:"amount"`
	Status         string    `json:"status"`
	Run            string    `json:"run"`
	StripeTransfer string    `json:"stripe_transfer,omitempty"`
	Failure        string    `json:"failure,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// RunReport is what a payout run did: the payout IDs paid, failed or left
// pending, and the creators passed over counted by reason.
type RunReport struct {
	Tenant  string         `json:"tenant"`
	Run     string         `json:"run"`
	Paid    []string       `json:"paid"`
	Failed  []string       `json:"failed"`
	Pending []string       `json:"pending"`
	Skipped map[string]int `json:"skipped"`
}

// Review is an item in the manual review queue.
type Review struct {
	ID         string    `json:"id"`
	Subject    string    `json:"subject"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail,omitempty"`
	Reference  string    `json:"reference,omitempty"`
	Status     string    `json:"status"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
	Resolution string    `json:"resolution,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AuditRecord is one line of the audit log.
type AuditRecord struct {
	ID      string    `json:"id"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Subject string    `json:"subject"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// UserExport is a data subject access export.
type UserExport struct {
	GeneratedAt  time.Time  `json:"generated_at"`
	Tenant       string     `json:"tenant"`
	User         string     `json:"user"`
	Wallet       Wallet     `json:"wallet"`
	Entries      []Entry    `json:"ledger_entries"`
	Purchases    []Purchase `json:"purchases"`
	TipsSent     []Tip      `json:"tips_sent"`
	TipsReceived []Tip      `json:"tips_received"`
	Creator      *Creator   `json:"creator,omitempty"`
	Payouts      []Payout   `json:"payouts"`
	Reviews      []Review   `json:"reviews"`
	HeldBy       []string   `json:"held_elsewhere"`
}

// Erasure is what an erasure request removed and what it kept.
type Erasure struct {
	TipMessagesErased int      `json:"tip_messages_erased"`
	Retained          []string `json:"retained"`
}

// TrialBalance is the sum of every posting per currency.
type TrialBalance struct {
	Balanced bool             `json:"balanced"`
	Sums     map[string]int64 `json:"sums"`
}
