// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package memory is store.Store in maps behind one mutex, for tests and
// development. It forgets everything on restart, which is why cmd/motom
// refuses it unless MOTOM_DEV is set.
package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// Store is an in-memory store.Store.
type Store struct {
	mu        sync.Mutex
	tenants   map[string]store.Tenant
	keys      map[string]store.Key
	accounts  map[ledger.AccountID]ledger.Account
	entries   []ledger.Entry
	byID      map[string]int
	byKey     map[string]int
	reversal  map[string]int
	balances  map[ledger.AccountID]ledger.Amount
	wallets   map[string]store.Wallet
	purchases map[string]store.Purchase
	tips      []store.Tip
	payees    map[string]store.Payee
	payouts   map[string]store.Payout
	reviews   map[string]store.Review
	webhooks  map[string]bool
	audit     []store.AuditRecord
}

var _ store.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{
		tenants: map[string]store.Tenant{}, keys: map[string]store.Key{},
		accounts: map[ledger.AccountID]ledger.Account{}, byID: map[string]int{}, byKey: map[string]int{},
		reversal: map[string]int{}, balances: map[ledger.AccountID]ledger.Amount{},
		wallets: map[string]store.Wallet{}, purchases: map[string]store.Purchase{},
		payees: map[string]store.Payee{}, payouts: map[string]store.Payout{},
		reviews: map[string]store.Review{}, webhooks: map[string]bool{},
	}
}

func pair(a, b string) string { return a + "\x00" + b }

func copyTenant(t store.Tenant) store.Tenant {
	t.Currencies = append([]ledger.Currency(nil), t.Currencies...)
	t.Config.Packs = append([]store.Pack(nil), t.Config.Packs...)
	return t
}

func copyEntry(e ledger.Entry) ledger.Entry {
	e.Postings = append([]ledger.Posting(nil), e.Postings...)
	if e.Metadata != nil {
		metadata := make(map[string]string, len(e.Metadata))
		for k, v := range e.Metadata {
			metadata[k] = v
		}
		e.Metadata = metadata
	}
	e.Expect = nil
	return e
}

func (s *Store) CreateTenant(_ context.Context, t store.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[t.ID]; ok {
		return store.ErrAlreadyExists
	}
	t.Version = 0
	s.tenants[t.ID] = copyTenant(t)
	return nil
}

func (s *Store) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tenants[id]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return copyTenant(t), nil
}

func (s *Store) SaveTenant(_ context.Context, t store.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.tenants[t.ID]
	if !ok {
		return store.ErrNotFound
	}
	if current.Version != t.Version {
		return store.ErrVersionConflict
	}
	t.Version++
	t.CreatedAt = current.CreatedAt
	s.tenants[t.ID] = copyTenant(t)
	return nil
}

func (s *Store) ListTenants(_ context.Context) ([]store.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Tenant
	for _, t := range s.tenants {
		out = append(out, copyTenant(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) PutKey(_ context.Context, k store.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[k.ID]; ok {
		return store.ErrAlreadyExists
	}
	k.Scopes = append([]store.Scope(nil), k.Scopes...)
	s.keys[k.ID] = k
	return nil
}

func (s *Store) GetKey(_ context.Context, id string) (store.Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return store.Key{}, store.ErrNotFound
	}
	k.Scopes = append([]store.Scope(nil), k.Scopes...)
	return k, nil
}

func (s *Store) ListKeys(_ context.Context, tenant string) ([]store.Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Key
	for _, k := range s.keys {
		if k.Tenant == tenant {
			k.Scopes = append([]store.Scope(nil), k.Scopes...)
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) RevokeKey(_ context.Context, tenant, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok || k.Tenant != tenant {
		return store.ErrNotFound
	}
	if !k.Revoked() {
		k.RevokedAt = at
		s.keys[id] = k
	}
	return nil
}

func (s *Store) EnsureAccount(_ context.Context, a ledger.Account) (ledger.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.accounts[a.ID]; ok {
		return existing, nil
	}
	parsed, err := ledger.NewAccount(a.ID, a.CreatedAt)
	if err != nil {
		return ledger.Account{}, err
	}
	s.accounts[a.ID] = parsed
	return parsed, nil
}

func (s *Store) GetAccount(_ context.Context, id ledger.AccountID) (ledger.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return ledger.Account{}, store.ErrNotFound
	}
	return a, nil
}

func (s *Store) ListAccounts(_ context.Context, tenant, owner string) ([]ledger.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ledger.Account
	for _, a := range s.accounts {
		if a.Tenant == tenant && (owner == "" || a.Owner == owner) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) Post(_ context.Context, e ledger.Entry) (ledger.Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return ledger.Entry{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index, ok := s.byKey[pair(e.Tenant, e.IdempotencyKey)]; ok {
		existing := s.entries[index]
		if existing.Fingerprint() != e.Fingerprint() {
			return ledger.Entry{}, false, store.ErrIdempotencyMismatch
		}
		return copyEntry(existing), false, nil
	}
	if _, ok := s.byID[e.ID]; ok {
		return ledger.Entry{}, false, store.ErrAlreadyExists
	}
	if e.Reverses != "" {
		index, ok := s.byID[e.Reverses]
		if !ok || s.entries[index].Tenant != e.Tenant {
			return ledger.Entry{}, false, store.ErrNotFound
		}
		if _, done := s.reversal[e.Reverses]; done {
			return ledger.Entry{}, false, store.ErrAlreadyReversed
		}
	}
	for _, p := range e.Postings {
		account, ok := s.accounts[p.Account]
		if !ok {
			return ledger.Entry{}, false, store.ErrNotFound
		}
		if expected, ok := e.Expect[p.Account]; ok && expected != account.Version {
			return ledger.Entry{}, false, store.ErrVersionConflict
		}
		if err := store.CheckOverdraw(account, s.balances[p.Account], p.Amount); err != nil {
			return ledger.Entry{}, false, err
		}
	}
	e = copyEntry(e)
	for _, p := range e.Postings {
		account := s.accounts[p.Account]
		account.Version++
		s.accounts[p.Account] = account
		s.balances[p.Account] += p.Amount
	}
	index := len(s.entries)
	s.entries = append(s.entries, e)
	s.byID[e.ID] = index
	s.byKey[pair(e.Tenant, e.IdempotencyKey)] = index
	if e.Reverses != "" {
		s.reversal[e.Reverses] = index
	}
	s.audit = append(s.audit, store.EntryAudit(e))
	return copyEntry(e), true, nil
}

func (s *Store) GetEntry(_ context.Context, tenant, id string) (ledger.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.byID[id]
	if !ok || s.entries[index].Tenant != tenant {
		return ledger.Entry{}, store.ErrNotFound
	}
	return copyEntry(s.entries[index]), nil
}

func (s *Store) FindEntry(_ context.Context, tenant, key string) (ledger.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.byKey[pair(tenant, key)]
	if !ok {
		return ledger.Entry{}, store.ErrNotFound
	}
	return copyEntry(s.entries[index]), nil
}

func (s *Store) FindReversal(_ context.Context, tenant, id string) (ledger.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.reversal[id]
	if !ok || s.entries[index].Tenant != tenant {
		return ledger.Entry{}, store.ErrNotFound
	}
	return copyEntry(s.entries[index]), nil
}

func (s *Store) Balance(_ context.Context, id ledger.AccountID) (store.Balance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return store.Balance{}, store.ErrNotFound
	}
	// Summed from the entries rather than read from the cache, so the
	// conformance suite checks the cache Post keeps for overdraft checks.
	var sum ledger.Amount
	for _, e := range s.entries {
		for _, p := range e.Postings {
			if p.Account == id {
				sum += p.Amount
			}
		}
	}
	return store.Balance{Amount: sum, Version: account.Version}, nil
}

func (s *Store) CreditsSince(_ context.Context, id ledger.AccountID, since time.Time) (ledger.Amount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return 0, store.ErrNotFound
	}
	var total ledger.Amount
	for _, e := range s.entries {
		if e.CreatedAt.Before(since) {
			continue
		}
		for _, p := range e.Postings {
			if p.Account == id && account.Normal(p.Amount) > 0 {
				total += account.Normal(p.Amount)
			}
		}
	}
	return total, nil
}

func touches(e ledger.Entry, f store.EntryFilter) bool {
	if f.Account == "" && f.Owner == "" {
		return true
	}
	for _, p := range e.Postings {
		if f.Account != "" && p.Account == f.Account {
			return true
		}
		if f.Owner != "" {
			if _, _, owner, _, err := p.Account.Parse(); err == nil && owner == f.Owner {
				return true
			}
		}
	}
	return false
}

func (s *Store) ListEntries(_ context.Context, f store.EntryFilter) ([]ledger.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ledger.Entry
	for i := len(s.entries) - 1; i >= 0 && (f.Limit <= 0 || len(out) < f.Limit); i-- {
		e := s.entries[i]
		if e.Tenant != f.Tenant || (f.Kind != "" && e.Kind != f.Kind) || !touches(e, f) {
			continue
		}
		out = append(out, copyEntry(e))
	}
	return out, nil
}

func (s *Store) TrialBalance(_ context.Context, tenant string) (map[ledger.Currency]ledger.Amount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[ledger.Currency]ledger.Amount{}
	for _, e := range s.entries {
		if e.Tenant != tenant {
			continue
		}
		for _, p := range e.Postings {
			_, _, _, currency, _ := p.Account.Parse()
			out[currency] += p.Amount
		}
	}
	return out, nil
}

func (s *Store) GetWallet(_ context.Context, tenant, user string) (store.Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wallets[pair(tenant, user)]
	if !ok {
		return store.Wallet{}, store.ErrNotFound
	}
	return w, nil
}

func (s *Store) SaveWallet(_ context.Context, w store.Wallet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := pair(w.Tenant, w.User)
	current, ok := s.wallets[id]
	switch {
	case !ok && w.Version != 0:
		return store.ErrNotFound
	case ok && current.Version != w.Version:
		return store.ErrVersionConflict
	case ok:
		w.CreatedAt = current.CreatedAt
	}
	w.Version++
	s.wallets[id] = w
	return nil
}

func (s *Store) CreatePurchase(_ context.Context, p store.Purchase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.purchases[p.ID]; ok {
		return store.ErrAlreadyExists
	}
	if p.StripeIntent != "" {
		for _, other := range s.purchases {
			if other.StripeIntent == p.StripeIntent {
				return store.ErrAlreadyExists
			}
		}
	}
	p.Version = 0
	s.purchases[p.ID] = p
	return nil
}

func (s *Store) GetPurchase(_ context.Context, tenant, id string) (store.Purchase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.purchases[id]
	if !ok || p.Tenant != tenant {
		return store.Purchase{}, store.ErrNotFound
	}
	return p, nil
}

func (s *Store) FindPurchaseByIntent(_ context.Context, intent string) (store.Purchase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.purchases {
		if intent != "" && p.StripeIntent == intent {
			return p, nil
		}
	}
	return store.Purchase{}, store.ErrNotFound
}

func (s *Store) SavePurchase(_ context.Context, p store.Purchase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.purchases[p.ID]
	if !ok || current.Tenant != p.Tenant {
		return store.ErrNotFound
	}
	if current.Version != p.Version {
		return store.ErrVersionConflict
	}
	if p.StripeIntent != "" && p.StripeIntent != current.StripeIntent {
		for id, other := range s.purchases {
			if id != p.ID && other.StripeIntent == p.StripeIntent {
				return store.ErrAlreadyExists
			}
		}
	}
	p.Version++
	p.CreatedAt = current.CreatedAt
	s.purchases[p.ID] = p
	return nil
}

func (s *Store) CountPurchases(_ context.Context, tenant, user string, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.purchases {
		if p.Tenant == tenant && p.User == user && !p.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (s *Store) ListPurchases(_ context.Context, tenant, user string) ([]store.Purchase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Purchase
	for _, p := range s.purchases {
		if p.Tenant == tenant && (user == "" || p.User == user) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *Store) CreateTip(_ context.Context, t store.Tip) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.tips {
		if existing.ID == t.ID {
			return store.ErrAlreadyExists
		}
	}
	s.tips = append(s.tips, t)
	return nil
}

func tipMatches(t store.Tip, q store.TipQuery) bool {
	return t.Tenant == q.Tenant && (q.Room == "" || t.Room == q.Room) &&
		(q.Sender == "" || t.Sender == q.Sender) && (q.Creator == "" || t.Creator == q.Creator) &&
		!t.CreatedAt.Before(q.Since)
}

func (s *Store) ListTips(_ context.Context, q store.TipQuery) ([]store.Tip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Tip
	for i := len(s.tips) - 1; i >= 0 && (q.Limit <= 0 || len(out) < q.Limit); i-- {
		if tipMatches(s.tips[i], q) {
			out = append(out, s.tips[i])
		}
	}
	return out, nil
}

func (s *Store) TipStats(_ context.Context, q store.TipQuery) (int, ledger.Amount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, sum := 0, ledger.Amount(0)
	for _, t := range s.tips {
		if tipMatches(t, q) {
			n++
			sum += t.Amount
		}
	}
	return n, sum, nil
}

func (s *Store) ScrubTips(_ context.Context, tenant, user string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i, t := range s.tips {
		if t.Tenant == tenant && t.Sender == user && t.Message != "" {
			s.tips[i].Message = ""
			n++
		}
	}
	return n, nil
}

func (s *Store) SavePayee(_ context.Context, p store.Payee) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := pair(p.Tenant, p.Creator)
	current, ok := s.payees[id]
	switch {
	case !ok && p.Version != 0:
		return store.ErrNotFound
	case ok && current.Version != p.Version:
		return store.ErrVersionConflict
	case ok:
		p.CreatedAt = current.CreatedAt
	}
	if p.StripeAccount != "" {
		for other, payee := range s.payees {
			if other != id && payee.StripeAccount == p.StripeAccount {
				return store.ErrAlreadyExists
			}
		}
	}
	p.Version++
	s.payees[id] = p
	return nil
}

func (s *Store) GetPayee(_ context.Context, tenant, creator string) (store.Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payees[pair(tenant, creator)]
	if !ok {
		return store.Payee{}, store.ErrNotFound
	}
	return p, nil
}

func (s *Store) FindPayeeByStripeAccount(_ context.Context, account string) (store.Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payees {
		if account != "" && p.StripeAccount == account {
			return p, nil
		}
	}
	return store.Payee{}, store.ErrNotFound
}

func (s *Store) ListPayees(_ context.Context, tenant string) ([]store.Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Payee
	for _, p := range s.payees {
		if p.Tenant == tenant {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Creator < out[j].Creator })
	return out, nil
}

func (s *Store) CreatePayout(_ context.Context, p store.Payout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.payouts[p.ID]; ok {
		return store.ErrAlreadyExists
	}
	p.Version = 0
	s.payouts[p.ID] = p
	return nil
}

func (s *Store) GetPayout(_ context.Context, tenant, id string) (store.Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payouts[id]
	if !ok || p.Tenant != tenant {
		return store.Payout{}, store.ErrNotFound
	}
	return p, nil
}

func (s *Store) FindPayoutByTransfer(_ context.Context, transfer string) (store.Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payouts {
		if transfer != "" && p.StripeTransfer == transfer {
			return p, nil
		}
	}
	return store.Payout{}, store.ErrNotFound
}

func (s *Store) SavePayout(_ context.Context, p store.Payout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.payouts[p.ID]
	if !ok || current.Tenant != p.Tenant {
		return store.ErrNotFound
	}
	if current.Version != p.Version {
		return store.ErrVersionConflict
	}
	p.Version++
	p.CreatedAt = current.CreatedAt
	s.payouts[p.ID] = p
	return nil
}

func (s *Store) ListPayouts(_ context.Context, q store.PayoutQuery) ([]store.Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Payout
	for _, p := range s.payouts {
		if p.Tenant == q.Tenant && (q.Creator == "" || p.Creator == q.Creator) && (q.Status == "" || p.Status == q.Status) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *Store) CreateReview(_ context.Context, r store.Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reviews[r.ID]; ok {
		return store.ErrAlreadyExists
	}
	r.Version = 0
	s.reviews[r.ID] = r
	return nil
}

func (s *Store) GetReview(_ context.Context, tenant, id string) (store.Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reviews[id]
	if !ok || r.Tenant != tenant {
		return store.Review{}, store.ErrNotFound
	}
	return r, nil
}

func (s *Store) SaveReview(_ context.Context, r store.Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.reviews[r.ID]
	if !ok || current.Tenant != r.Tenant {
		return store.ErrNotFound
	}
	if current.Version != r.Version {
		return store.ErrVersionConflict
	}
	r.Version++
	r.CreatedAt = current.CreatedAt
	s.reviews[r.ID] = r
	return nil
}

func (s *Store) ListReviews(_ context.Context, tenant string, status store.ReviewStatus, subject string) ([]store.Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Review
	for _, r := range s.reviews {
		if r.Tenant == tenant && (status == "" || r.Status == status) && (subject == "" || r.Subject == subject) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) RecordWebhook(_ context.Context, id, _ string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		return false, errors.New("a webhook event needs an id")
	}
	if s.webhooks[id] {
		return false, nil
	}
	s.webhooks[id] = true
	return true, nil
}

func (s *Store) ForgetWebhook(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.webhooks, id)
	return nil
}

func (s *Store) AppendAudit(_ context.Context, r store.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.audit {
		if existing.ID == r.ID {
			return store.ErrAlreadyExists
		}
	}
	s.audit = append(s.audit, r)
	return nil
}

func (s *Store) ListAudit(_ context.Context, tenant, subject string, limit int) ([]store.AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AuditRecord
	for i := len(s.audit) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		r := s.audit[i]
		if r.Tenant == tenant && (subject == "" || r.Subject == subject || strings.HasPrefix(r.Subject, subject+":")) {
			out = append(out, r)
		}
	}
	return out, nil
}
