// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package ledger is MOTOM's double-entry bookkeeping: accounts, immutable
// journal entries that always balance, and reversals in place of edits.
//
// Amounts are integer minor units of an account's currency (cents for USD,
// yen for JPY). A posting is signed: positive is a debit, negative a credit,
// and the postings of an entry sum to zero in every currency they touch. An
// account's balance is the sum of its postings and nothing else; there is no
// stored running total to disagree with the entries that produced it.
//
// The package is pure: it validates and constructs, and the store enforces
// what needs a transaction (idempotency, versions, overdraft).
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Amount is a quantity of minor units.
type Amount int64

// MaxAmount bounds any single posting. It is far beyond any real payment and
// small enough that a fee calculation (amount × basis points) cannot overflow.
const MaxAmount Amount = 100_000_000_000_000

// Currency is a lowercase ISO 4217 code, as Stripe writes it.
type Currency string

// Valid reports whether c is three lowercase ASCII letters.
func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// AccountKind is what an account represents.
type AccountKind string

const (
	// StripeClearing is money held for MOTOM in its Stripe balance. Asset.
	StripeClearing AccountKind = "stripe_clearing"
	// Wallet is credit a tenant's end user has bought and not yet spent.
	// Liability to the user.
	Wallet AccountKind = "wallet"
	// Earnings is what a creator has been tipped and not yet been paid.
	// Liability to the creator.
	Earnings AccountKind = "earnings"
	// PendingPayout is earnings committed to a payout that Stripe has not
	// yet confirmed. Liability to the creator.
	PendingPayout AccountKind = "pending_payout"
	// Fees is MOTOM's platform fee revenue.
	Fees AccountKind = "fees"
	// Tenant is the tenant's share: its fee on tips and spends, less any
	// promotional credit it funds.
	Tenant AccountKind = "tenant"
	// Platform is MOTOM's own operating account: chargeback losses and
	// write-offs land here.
	Platform AccountKind = "platform"
)

// Kinds lists every account kind.
var Kinds = []AccountKind{StripeClearing, Wallet, Earnings, PendingPayout, Fees, Tenant, Platform}

// Valid reports whether k is a known kind.
func (k AccountKind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// DebitNormal reports whether the kind's balance is naturally a debit. Only
// the clearing account is an asset; everything else is owed to someone or is
// revenue.
func (k AccountKind) DebitNormal() bool { return k == StripeClearing || k == Platform }

// Owned reports whether accounts of this kind belong to one user or creator
// rather than to the tenant as a whole.
func (k AccountKind) Owned() bool { return k == Wallet || k == Earnings || k == PendingPayout }

// MayOverdraw reports whether the kind's normal balance may go below zero.
// A wallet, earnings or pending payout below zero would mean somebody spent
// or was paid money they did not have.
func (k AccountKind) MayOverdraw() bool { return !k.Owned() }

// AccountID names an account: tenant/kind/owner/currency. Deterministic, so
// any part of the system can name an account without looking it up.
type AccountID string

const noOwner = "-"

// MaxSegment bounds each part of an account ID.
const MaxSegment = 128

// ValidSegment reports whether s can be part of an account ID: non-empty,
// bounded, and free of separators and control characters.
func ValidSegment(s string) bool {
	if s == "" || len(s) > MaxSegment {
		return false
	}
	for _, r := range s {
		if r == '/' || r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// AccountFor names an account. owner is empty for tenant-wide kinds.
func AccountFor(tenant string, kind AccountKind, owner string, currency Currency) AccountID {
	if owner == "" {
		owner = noOwner
	}
	return AccountID(tenant + "/" + string(kind) + "/" + owner + "/" + string(currency))
}

// Parse splits an account ID into its parts.
func (id AccountID) Parse() (tenant string, kind AccountKind, owner string, currency Currency, err error) {
	parts := strings.Split(string(id), "/")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("%w: account %q is not tenant/kind/owner/currency", ErrInvalid, id)
	}
	tenant, kind, owner, currency = parts[0], AccountKind(parts[1]), parts[2], Currency(parts[3])
	if !ValidSegment(tenant) || !kind.Valid() || !ValidSegment(owner) || !currency.Valid() {
		return "", "", "", "", fmt.Errorf("%w: account %q", ErrInvalid, id)
	}
	if kind.Owned() == (owner == noOwner) {
		return "", "", "", "", fmt.Errorf("%w: account %q: %s accounts %s an owner", ErrInvalid, id, kind, map[bool]string{true: "need", false: "take no"}[kind.Owned()])
	}
	if owner == noOwner {
		owner = ""
	}
	return tenant, kind, owner, currency, nil
}

// Account is a ledger account. Version advances on every posting, which is
// what optimistic concurrency compares.
type Account struct {
	ID        AccountID
	Tenant    string
	Kind      AccountKind
	Owner     string
	Currency  Currency
	Version   int64
	CreatedAt time.Time
}

// NewAccount builds an account from its ID.
func NewAccount(id AccountID, at time.Time) (Account, error) {
	tenant, kind, owner, currency, err := id.Parse()
	if err != nil {
		return Account{}, err
	}
	return Account{ID: id, Tenant: tenant, Kind: kind, Owner: owner, Currency: currency, CreatedAt: at}, nil
}

// Normal converts a signed (debit-positive) sum into the account's natural
// balance: what the user holds, what the creator is owed.
func (a Account) Normal(signed Amount) Amount {
	if a.Kind.DebitNormal() {
		return signed
	}
	return -signed
}

// Posting is one line of an entry.
type Posting struct {
	Account AccountID
	// Amount is positive for a debit and negative for a credit.
	Amount Amount
}

// EntryKind is why money moved.
type EntryKind string

const (
	KindPurchase          EntryKind = "purchase"
	KindSpend             EntryKind = "spend"
	KindGrant             EntryKind = "grant"
	KindTip               EntryKind = "tip"
	KindRefund            EntryKind = "refund"
	KindChargeback        EntryKind = "chargeback"
	KindPayoutInitiated   EntryKind = "payout_initiated"
	KindPayoutTransferred EntryKind = "payout_transferred"
	KindPayoutReturned    EntryKind = "payout_returned"
	KindReversal          EntryKind = "reversal"
	KindAdjustment        EntryKind = "adjustment"
)

// Entry is an immutable journal entry.
type Entry struct {
	ID     string
	Tenant string
	Kind   EntryKind
	// IdempotencyKey is unique per tenant. Posting an entry whose key exists
	// returns the existing entry if it is the same movement, and fails if it
	// is a different one.
	IdempotencyKey string
	Postings       []Posting
	// Reverses names the entry this one undoes. An entry is reversed at most
	// once.
	Reverses string
	// Reference names the outside thing this came from: a payment intent, a
	// dispute, a transfer, a tip.
	Reference string
	// Actor is who caused it: an API key ID, "stripe", "scheduler".
	Actor     string
	Memo      string
	Metadata  map[string]string
	CreatedAt time.Time

	// Expect, when set, is the version each named account must be at for the
	// entry to post. Not persisted.
	Expect map[AccountID]int64
}

var (
	// ErrInvalid is an entry or account that is malformed.
	ErrInvalid = errors.New("invalid ledger entry")
	// ErrUnbalanced is an entry whose postings do not sum to zero.
	ErrUnbalanced = errors.New("ledger entry does not balance")
)

// MaxPostings bounds the lines in one entry.
const MaxPostings = 32

// Validate checks everything about an entry that needs no store: shape,
// ownership, currency, bounds and balance.
func (e Entry) Validate() error {
	if !ValidSegment(e.Tenant) {
		return fmt.Errorf("%w: tenant", ErrInvalid)
	}
	if e.ID == "" || e.IdempotencyKey == "" || len(e.IdempotencyKey) > 255 || e.Kind == "" {
		return fmt.Errorf("%w: id, kind and an idempotency key of at most 255 bytes are required", ErrInvalid)
	}
	if len(e.Postings) < 2 || len(e.Postings) > MaxPostings {
		return fmt.Errorf("%w: an entry has between 2 and %d postings", ErrInvalid, MaxPostings)
	}
	sums := map[Currency]Amount{}
	seen := map[AccountID]bool{}
	for _, p := range e.Postings {
		tenant, _, _, currency, err := p.Account.Parse()
		if err != nil {
			return err
		}
		if tenant != e.Tenant {
			return fmt.Errorf("%w: account %s belongs to another tenant", ErrInvalid, p.Account)
		}
		if seen[p.Account] {
			return fmt.Errorf("%w: account %s appears twice", ErrInvalid, p.Account)
		}
		seen[p.Account] = true
		if p.Amount == 0 || p.Amount > MaxAmount || p.Amount < -MaxAmount {
			return fmt.Errorf("%w: posting to %s of %d is zero or out of range", ErrInvalid, p.Account, p.Amount)
		}
		// Bounded postings and a bounded count cannot overflow int64.
		sums[currency] += p.Amount
	}
	for currency, sum := range sums {
		if sum != 0 {
			return fmt.Errorf("%w: %s postings sum to %d", ErrUnbalanced, currency, sum)
		}
	}
	return nil
}

// Fingerprint identifies the movement an entry makes, independent of its ID
// and time, so a replayed idempotency key can be checked for a changed body.
func (e Entry) Fingerprint() string {
	lines := make([]string, 0, len(e.Postings))
	for _, p := range e.Postings {
		lines = append(lines, string(p.Account)+"="+strconv.FormatInt(int64(p.Amount), 10))
	}
	sort.Strings(lines)
	digest := sha256.Sum256([]byte(string(e.Kind) + "|" + e.Reverses + "|" + strings.Join(lines, ",")))
	return hex.EncodeToString(digest[:])
}

// Accounts lists the accounts an entry touches, sorted, which is the order a
// store locks them in so two entries cannot deadlock.
func (e Entry) Accounts() []AccountID {
	ids := make([]AccountID, 0, len(e.Postings))
	for _, p := range e.Postings {
		ids = append(ids, p.Account)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Reverse builds the entry that undoes original: the same postings with the
// signs flipped.
func Reverse(original Entry, id, idempotencyKey, actor, memo string, at time.Time) Entry {
	postings := make([]Posting, len(original.Postings))
	for i, p := range original.Postings {
		postings[i] = Posting{Account: p.Account, Amount: -p.Amount}
	}
	return Entry{
		ID: id, Tenant: original.Tenant, Kind: KindReversal, IdempotencyKey: idempotencyKey,
		Postings: postings, Reverses: original.ID, Reference: original.Reference,
		Actor: actor, Memo: memo, CreatedAt: at,
	}
}

// BasisPoints is a fraction in ten-thousandths.
type BasisPoints int

// Fee is amount × bps / 10000, rounded down: the party paid the remainder
// gets the fraction of a minor unit, never the fee-taker.
func Fee(amount Amount, bps BasisPoints) Amount {
	if amount <= 0 || bps <= 0 {
		return 0
	}
	return amount * Amount(bps) / 10_000
}

// Split divides amount into the fees at each rate and the remainder. The
// parts always sum to amount.
func Split(amount Amount, rates ...BasisPoints) (fees []Amount, remainder Amount) {
	remainder = amount
	for _, bps := range rates {
		fee := Fee(amount, bps)
		fees = append(fees, fee)
		remainder -= fee
	}
	return fees, remainder
}
