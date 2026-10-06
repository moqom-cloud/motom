// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// Credits are denominated in minor units of the wallet's currency: one
// credit is one cent of a USD wallet. A tenant's app may display them in any
// unit it likes; the ledger holds money.

// WalletState is a wallet and its balance in each currency.
type WalletState struct {
	Wallet   store.Wallet
	Balances map[ledger.Currency]ledger.Amount
}

func (s *Service) ensureWallet(ctx context.Context, tenant, user string) (store.Wallet, error) {
	if err := validID("user", user); err != nil {
		return store.Wallet{}, err
	}
	w, err := s.store.GetWallet(ctx, tenant, user)
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Wallet{}, err
	}
	now := s.now()
	err = s.store.SaveWallet(ctx, store.Wallet{Tenant: tenant, User: user, CreatedAt: now, UpdatedAt: now})
	if err != nil && !errors.Is(err, store.ErrVersionConflict) {
		return store.Wallet{}, err
	}
	return s.store.GetWallet(ctx, tenant, user)
}

// Wallet returns a user's wallet, creating it if this is the first look.
func (s *Service) Wallet(ctx context.Context, tenant, user string) (WalletState, error) {
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return WalletState{}, err
	}
	w, err := s.ensureWallet(ctx, tenant, user)
	if err != nil {
		return WalletState{}, err
	}
	state := WalletState{Wallet: w, Balances: map[ledger.Currency]ledger.Amount{}}
	for _, c := range t.Currencies {
		state.Balances[c] = 0
	}
	accounts, err := s.store.ListAccounts(ctx, tenant, user)
	if err != nil {
		return WalletState{}, err
	}
	for _, a := range accounts {
		if a.Kind != ledger.Wallet {
			continue
		}
		b, err := s.store.Balance(ctx, a.ID)
		if err != nil {
			return WalletState{}, err
		}
		state.Balances[a.Currency] = a.Normal(b.Amount)
	}
	return state, nil
}

// WalletEntries lists the entries touching a user's accounts.
func (s *Service) WalletEntries(ctx context.Context, tenant, user string, limit int) ([]ledger.Entry, error) {
	if err := validID("user", user); err != nil {
		return nil, err
	}
	return s.store.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Owner: user, Limit: limit})
}

// replay returns the entry an idempotency key already produced, if any.
func (s *Service) replay(ctx context.Context, tenant, key string) (ledger.Entry, bool, error) {
	e, err := s.store.FindEntry(ctx, tenant, key)
	if errors.Is(err, store.ErrNotFound) {
		return ledger.Entry{}, false, nil
	}
	return e, err == nil, err
}

// sameRequest reports whether a replayed entry is the one this request would
// have posted: it moved amount (in currency, when given) through user's
// wallet and carries the metadata. A key reused for a different request is
// refused rather than answered with the first request's result.
func sameRequest(e ledger.Entry, user string, amount ledger.Amount, currency ledger.Currency, metadata map[string]string) error {
	for k, v := range metadata {
		if e.Metadata[k] != v {
			return store.ErrIdempotencyMismatch
		}
	}
	for _, p := range e.Postings {
		_, kind, owner, c, err := p.Account.Parse()
		if err != nil || kind != ledger.Wallet || owner != user {
			continue
		}
		if (p.Amount == amount || p.Amount == -amount) && (currency == "" || strings.EqualFold(string(c), string(currency))) {
			return nil
		}
	}
	return store.ErrIdempotencyMismatch
}

// SpendRequest consumes credit for something in the tenant's app.
type SpendRequest struct {
	User           string
	Amount         ledger.Amount
	Currency       ledger.Currency
	Reference      string
	Memo           string
	IdempotencyKey string
}

// Spend debits a wallet. The tenant earns the spend less MOTOM's platform fee.
func (s *Service) Spend(ctx context.Context, actor, tenant string, r SpendRequest) (ledger.Entry, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return ledger.Entry{}, err
	}
	key := "spend:" + r.IdempotencyKey
	if e, ok, err := s.replay(ctx, tenant, key); err != nil {
		return ledger.Entry{}, err
	} else if ok {
		return e, sameRequest(e, r.User, r.Amount, r.Currency, nil)
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return ledger.Entry{}, err
	}
	currency, err := s.currency(t, r.Currency)
	if err != nil {
		return ledger.Entry{}, err
	}
	if r.Amount <= 0 || r.Amount > ledger.MaxAmount || len(r.Reference) > 200 || len(r.Memo) > 200 {
		return ledger.Entry{}, fmt.Errorf("%w: a positive amount, and reference and memo of at most 200 bytes", ErrInvalid)
	}
	w, err := s.ensureWallet(ctx, tenant, r.User)
	if err != nil {
		return ledger.Entry{}, err
	}
	if w.Frozen {
		return ledger.Entry{}, ErrFrozen
	}
	ids, err := s.accounts(ctx, tenant, currency,
		accountSpec{ledger.Wallet, r.User}, accountSpec{ledger.Fees, ""}, accountSpec{ledger.Tenant, ""})
	if err != nil {
		return ledger.Entry{}, err
	}
	fees, rest := ledger.Split(r.Amount, t.PlatformFee)
	lines := (&postings{}).add(ids[0], r.Amount).add(ids[1], -fees[0]).add(ids[2], -rest)
	e, _, err := s.post(ctx, ledger.Entry{Tenant: tenant, Kind: ledger.KindSpend, IdempotencyKey: key,
		Postings: lines.lines(), Reference: r.Reference, Actor: actor, Memo: r.Memo,
		Metadata: map[string]string{"user": r.User}})
	return e, err
}

// GrantRequest gives a user promotional credit the tenant pays for.
type GrantRequest struct {
	User           string
	Amount         ledger.Amount
	Currency       ledger.Currency
	Memo           string
	IdempotencyKey string
}

// Grant credits a wallet from the tenant's account.
func (s *Service) Grant(ctx context.Context, actor, tenant string, r GrantRequest) (ledger.Entry, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return ledger.Entry{}, err
	}
	key := "grant:" + r.IdempotencyKey
	if e, ok, err := s.replay(ctx, tenant, key); err != nil {
		return ledger.Entry{}, err
	} else if ok {
		return e, sameRequest(e, r.User, r.Amount, r.Currency, nil)
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return ledger.Entry{}, err
	}
	currency, err := s.currency(t, r.Currency)
	if err != nil {
		return ledger.Entry{}, err
	}
	if r.Amount <= 0 || r.Amount > MaxPackPrice || len(r.Memo) > 200 {
		return ledger.Entry{}, fmt.Errorf("%w: a grant is 1-%d", ErrInvalid, MaxPackPrice)
	}
	if _, err := s.ensureWallet(ctx, tenant, r.User); err != nil {
		return ledger.Entry{}, err
	}
	ids, err := s.accounts(ctx, tenant, currency, accountSpec{ledger.Tenant, ""}, accountSpec{ledger.Wallet, r.User})
	if err != nil {
		return ledger.Entry{}, err
	}
	e, _, err := s.post(ctx, ledger.Entry{Tenant: tenant, Kind: ledger.KindGrant, IdempotencyKey: key,
		Postings: []ledger.Posting{{Account: ids[0], Amount: r.Amount}, {Account: ids[1], Amount: -r.Amount}},
		Actor:    actor, Memo: r.Memo, Metadata: map[string]string{"user": r.User}})
	return e, err
}

// SetFrozen freezes or unfreezes a wallet. A frozen wallet keeps its money
// and can receive it, but cannot spend, tip or buy.
func (s *Service) SetFrozen(ctx context.Context, actor, tenant, user string, frozen bool, reason string) (store.Wallet, error) {
	if len(reason) > 200 {
		return store.Wallet{}, fmt.Errorf("%w: reason longer than 200 bytes", ErrInvalid)
	}
	for attempt := 0; attempt < 5; attempt++ {
		w, err := s.ensureWallet(ctx, tenant, user)
		if err != nil {
			return store.Wallet{}, err
		}
		if w.Frozen == frozen && (!frozen || w.FrozenReason == reason) {
			return w, nil
		}
		w.Frozen, w.FrozenReason, w.UpdatedAt = frozen, reason, s.now()
		if !frozen {
			w.FrozenReason = ""
		}
		err = s.store.SaveWallet(ctx, w)
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return store.Wallet{}, err
		}
		action := "wallet.unfreeze"
		if frozen {
			action = "wallet.freeze"
		}
		s.audit(ctx, tenant, actor, action, "wallet:"+user, fmt.Sprintf("%q", reason))
		return s.store.GetWallet(ctx, tenant, user)
	}
	return store.Wallet{}, ErrConflict
}
