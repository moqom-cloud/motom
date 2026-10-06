// Copyright 2026 Elliott Wave LLC. All rights reserved.

package httpapi

import (
	"time"

	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// The JSON the API speaks. The client package declares the same shapes; a
// test in client holds the two together.

type postingView struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

type entryView struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Postings  []postingView     `json:"postings"`
	Reverses  string            `json:"reverses,omitempty"`
	Reference string            `json:"reference,omitempty"`
	Memo      string            `json:"memo,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

func viewEntry(e ledger.Entry) entryView {
	v := entryView{ID: e.ID, Kind: string(e.Kind), Reverses: e.Reverses, Reference: e.Reference, Memo: e.Memo,
		Metadata: e.Metadata, CreatedAt: e.CreatedAt, Postings: make([]postingView, 0, len(e.Postings))}
	for _, p := range e.Postings {
		v.Postings = append(v.Postings, postingView{Account: string(p.Account), Amount: int64(p.Amount)})
	}
	return v
}

func viewEntries(entries []ledger.Entry) []entryView {
	out := make([]entryView, 0, len(entries))
	for _, e := range entries {
		out = append(out, viewEntry(e))
	}
	return out
}

type walletView struct {
	User         string           `json:"user"`
	Frozen       bool             `json:"frozen"`
	FrozenReason string           `json:"frozen_reason,omitempty"`
	Balances     map[string]int64 `json:"balances"`
	CreatedAt    time.Time        `json:"created_at"`
}

func viewWallet(w service.WalletState) walletView {
	v := walletView{User: w.Wallet.User, Frozen: w.Wallet.Frozen, FrozenReason: w.Wallet.FrozenReason,
		Balances: map[string]int64{}, CreatedAt: w.Wallet.CreatedAt}
	for c, a := range w.Balances {
		v.Balances[string(c)] = int64(a)
	}
	return v
}

type purchaseView struct {
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

func viewPurchase(p store.Purchase) purchaseView {
	return purchaseView{ID: p.ID, User: p.User, Pack: p.Pack, Currency: string(p.Currency), Price: int64(p.Price),
		Credits: int64(p.Credits), Refunded: int64(p.Refunded), Status: string(p.Status), Mode: p.Mode, CreatedAt: p.CreatedAt}
}

type tipView struct {
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

func viewTip(t store.Tip) tipView {
	return tipView{ID: t.ID, Room: t.Room, Sender: t.Sender, Creator: t.Creator, Amount: int64(t.Amount),
		Currency: string(t.Currency), Message: t.Message, EntryID: t.EntryID, CreatedAt: t.CreatedAt}
}

func viewTips(tips []store.Tip) []tipView {
	out := make([]tipView, 0, len(tips))
	for _, t := range tips {
		out = append(out, viewTip(t))
	}
	return out
}

type earningsView struct {
	Currency  string `json:"currency"`
	Balance   int64  `json:"balance"`
	Held      int64  `json:"held"`
	Available int64  `json:"available"`
	Pending   int64  `json:"pending"`
}

type creatorView struct {
	Creator        string         `json:"creator"`
	StripeAccount  string         `json:"stripe_account,omitempty"`
	KYC            string         `json:"kyc"`
	PayoutsEnabled bool           `json:"payouts_enabled"`
	DisabledReason string         `json:"disabled_reason,omitempty"`
	Frozen         bool           `json:"frozen"`
	Payable        bool           `json:"payable"`
	OpenReviews    int            `json:"open_reviews"`
	Earnings       []earningsView `json:"earnings"`
}

func viewCreator(s service.PayeeState) creatorView {
	p := s.Payee
	v := creatorView{Creator: p.Creator, StripeAccount: p.StripeAccount, KYC: string(p.KYC), PayoutsEnabled: p.PayoutsEnabled,
		DisabledReason: p.DisabledReason, Frozen: p.Frozen, Payable: p.Payable(), OpenReviews: s.OpenReviews,
		Earnings: make([]earningsView, 0, len(s.Earnings))}
	for _, e := range s.Earnings {
		v.Earnings = append(v.Earnings, earningsView{Currency: string(e.Currency), Balance: int64(e.Balance),
			Held: int64(e.Held), Available: int64(e.Available), Pending: int64(e.Pending)})
	}
	return v
}

type payoutView struct {
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

func viewPayouts(payouts []store.Payout) []payoutView {
	out := make([]payoutView, 0, len(payouts))
	for _, p := range payouts {
		out = append(out, payoutView{ID: p.ID, Creator: p.Creator, Currency: string(p.Currency), Amount: int64(p.Amount),
			Status: string(p.Status), Run: p.Run, StripeTransfer: p.StripeTransfer, Failure: p.Failure, CreatedAt: p.CreatedAt})
	}
	return out
}

type reviewView struct {
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

func viewReview(r store.Review) reviewView {
	return reviewView{ID: r.ID, Subject: r.Subject, Reason: r.Reason, Detail: r.Detail, Reference: r.Reference,
		Status: string(r.Status), ResolvedBy: r.ResolvedBy, Resolution: r.Resolution, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func viewReviews(reviews []store.Review) []reviewView {
	out := make([]reviewView, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, viewReview(r))
	}
	return out
}

type keyView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func viewKey(k store.Key) keyView {
	v := keyView{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, Scopes: make([]string, 0, len(k.Scopes))}
	for _, s := range k.Scopes {
		v.Scopes = append(v.Scopes, string(s))
	}
	if k.Revoked() {
		at := k.RevokedAt
		v.RevokedAt = &at
	}
	return v
}

type tenantView struct {
	ID          string             `json:"id"`
	Currencies  []string           `json:"currencies"`
	PlatformFee int                `json:"platform_fee_bps"`
	Config      store.TenantConfig `json:"config"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

func viewTenant(t store.Tenant) tenantView {
	v := tenantView{ID: t.ID, PlatformFee: int(t.PlatformFee), Config: t.Config, UpdatedAt: t.UpdatedAt}
	for _, c := range t.Currencies {
		v.Currencies = append(v.Currencies, string(c))
	}
	if v.Config.Packs == nil {
		v.Config.Packs = []store.Pack{}
	}
	return v
}

type auditView struct {
	ID      string    `json:"id"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Subject string    `json:"subject"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

type exportView struct {
	GeneratedAt  time.Time      `json:"generated_at"`
	Tenant       string         `json:"tenant"`
	User         string         `json:"user"`
	Wallet       walletView     `json:"wallet"`
	Entries      []entryView    `json:"ledger_entries"`
	Purchases    []purchaseView `json:"purchases"`
	TipsSent     []tipView      `json:"tips_sent"`
	TipsReceived []tipView      `json:"tips_received"`
	Creator      *creatorView   `json:"creator,omitempty"`
	Payouts      []payoutView   `json:"payouts"`
	Reviews      []reviewView   `json:"reviews"`
	HeldBy       []string       `json:"held_elsewhere"`
}
