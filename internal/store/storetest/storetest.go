// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package storetest is the contract every store.Store keeps, run against
// each implementation so memory cannot drift from Postgres.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

var at = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const tenant = "t_1"

var (
	clearing = ledger.AccountFor(tenant, ledger.StripeClearing, "", "usd")
	fees     = ledger.AccountFor(tenant, ledger.Fees, "", "usd")
	tenantAc = ledger.AccountFor(tenant, ledger.Tenant, "", "usd")
	walletA  = ledger.AccountFor(tenant, ledger.Wallet, "u_a", "usd")
	walletB  = ledger.AccountFor(tenant, ledger.Wallet, "u_b", "usd")
	earnings = ledger.AccountFor(tenant, ledger.Earnings, "c_1", "usd")
)

// Run exercises a store. open returns an empty store per case.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	ctx := context.Background()
	var ids atomic.Int64
	id := func(prefix string) string { return fmt.Sprintf("%s_%d", prefix, ids.Add(1)) }

	ensure := func(t *testing.T, s store.Store, accounts ...ledger.AccountID) {
		t.Helper()
		for _, a := range accounts {
			if _, err := s.EnsureAccount(ctx, ledger.Account{ID: a, CreatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry := func(kind ledger.EntryKind, key string, when time.Time, postings ...ledger.Posting) ledger.Entry {
		return ledger.Entry{ID: id("je"), Tenant: tenant, Kind: kind, IdempotencyKey: key,
			Postings: postings, Actor: "test", CreatedAt: when}
	}
	fund := func(t *testing.T, s store.Store, wallet ledger.AccountID, amount ledger.Amount) ledger.Entry {
		t.Helper()
		e, created, err := s.Post(ctx, entry(ledger.KindPurchase, id("fund"), at,
			ledger.Posting{Account: clearing, Amount: amount}, ledger.Posting{Account: wallet, Amount: -amount}))
		if err != nil || !created {
			t.Fatalf("fund: %v %v", created, err)
		}
		return e
	}
	balance := func(t *testing.T, s store.Store, a ledger.AccountID) store.Balance {
		t.Helper()
		b, err := s.Balance(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("tenants", func(t *testing.T) {
		s := open(t)
		tn := store.Tenant{ID: tenant, Currencies: []ledger.Currency{"usd", "eur"}, PlatformFee: 500,
			Config:    store.TenantConfig{TenantFee: 1000, Packs: []store.Pack{{ID: "p1", Name: "Small", Currency: "usd", Price: 500, Bonus: 50}}, HoldDays: 7},
			CreatedAt: at, UpdatedAt: at}
		if err := s.CreateTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTenant(ctx, tn); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate: %v", err)
		}
		got, err := s.GetTenant(ctx, tenant)
		if err != nil || got.PlatformFee != 500 || len(got.Currencies) != 2 || len(got.Config.Packs) != 1 ||
			got.Config.Packs[0].Bonus != 50 || got.Config.TenantFee != 1000 || got.Version != 0 {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		got.Config.HoldDays = 14
		if err := s.SaveTenant(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTenant(ctx, got); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("stale save: %v", err)
		}
		again, _ := s.GetTenant(ctx, tenant)
		if again.Config.HoldDays != 14 || again.Version != 1 {
			t.Fatalf("saved: %+v", again)
		}
		if _, err := s.GetTenant(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		list, err := s.ListTenants(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %v %v", list, err)
		}
	})

	t.Run("keys", func(t *testing.T) {
		s := open(t)
		k := store.Key{ID: "k1", Tenant: tenant, Name: "backend", Scopes: []store.Scope{store.ScopeTipsWrite}, Hash: "h", CreatedAt: at}
		if err := s.PutKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		if err := s.PutKey(ctx, k); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate key: %v", err)
		}
		got, err := s.GetKey(ctx, "k1")
		if err != nil || got.Hash != "h" || len(got.Scopes) != 1 || got.Scopes[0] != store.ScopeTipsWrite {
			t.Fatalf("key: %+v %v", got, err)
		}
		if err := s.RevokeKey(ctx, "t_other", "k1", at); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-tenant revoke: %v", err)
		}
		if err := s.RevokeKey(ctx, tenant, "k1", at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetKey(ctx, "k1")
		if !got.Revoked() {
			t.Fatal("not revoked")
		}
		list, _ := s.ListKeys(ctx, tenant)
		if len(list) != 1 {
			t.Fatalf("list: %v", list)
		}
	})

	t.Run("accounts", func(t *testing.T) {
		s := open(t)
		ensure(t, s, walletA, walletA)
		a, err := s.GetAccount(ctx, walletA)
		if err != nil || a.Kind != ledger.Wallet || a.Owner != "u_a" || a.Currency != "usd" || a.Tenant != tenant {
			t.Fatalf("account: %+v %v", a, err)
		}
		if _, err := s.EnsureAccount(ctx, ledger.Account{ID: "bad"}); err == nil {
			t.Fatal("malformed account created")
		}
		if _, err := s.GetAccount(ctx, walletB); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		ensure(t, s, earnings, fees)
		owned, _ := s.ListAccounts(ctx, tenant, "u_a")
		all, _ := s.ListAccounts(ctx, tenant, "")
		if len(owned) != 1 || len(all) != 3 {
			t.Fatalf("list: %d %d", len(owned), len(all))
		}
	})

	t.Run("posting and idempotency", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA)
		e := entry(ledger.KindPurchase, "pi_1", at,
			ledger.Posting{Account: clearing, Amount: 1000}, ledger.Posting{Account: walletA, Amount: -1000})
		e.Reference, e.Memo, e.Metadata = "pi_1", "top-up", map[string]string{"pack": "p1"}
		stored, created, err := s.Post(ctx, e)
		if err != nil || !created || stored.ID != e.ID {
			t.Fatalf("post: %v %v", created, err)
		}
		replay := e
		replay.ID = id("je")
		again, created, err := s.Post(ctx, replay)
		if err != nil || created || again.ID != e.ID {
			t.Fatalf("replay: %v %v %s", created, err, again.ID)
		}
		changed := replay
		changed.Postings = []ledger.Posting{{Account: clearing, Amount: 2000}, {Account: walletA, Amount: -2000}}
		if _, _, err := s.Post(ctx, changed); !errors.Is(err, store.ErrIdempotencyMismatch) {
			t.Fatalf("changed replay: %v", err)
		}
		if b := balance(t, s, walletA); b.Amount != -1000 || b.Version != 1 {
			t.Fatalf("balance: %+v", b)
		}
		found, err := s.FindEntry(ctx, tenant, "pi_1")
		if err != nil || found.ID != e.ID || found.Metadata["pack"] != "p1" || found.Memo != "top-up" ||
			found.Reference != "pi_1" || !found.CreatedAt.Equal(at) || len(found.Postings) != 2 {
			t.Fatalf("find: %+v %v", found, err)
		}
		if _, err := s.GetEntry(ctx, "t_other", e.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-tenant read: %v", err)
		}
		unknown := entry(ledger.KindPurchase, "x", at,
			ledger.Posting{Account: clearing, Amount: 1}, ledger.Posting{Account: walletB, Amount: -1})
		if _, _, err := s.Post(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown account: %v", err)
		}
		unbalanced := entry(ledger.KindPurchase, "y", at,
			ledger.Posting{Account: clearing, Amount: 2}, ledger.Posting{Account: walletA, Amount: -1})
		if _, _, err := s.Post(ctx, unbalanced); !errors.Is(err, ledger.ErrUnbalanced) {
			t.Fatalf("unbalanced: %v", err)
		}
		audit, err := s.ListAudit(ctx, tenant, "entry:"+e.ID, 0)
		if err != nil || len(audit) != 1 || audit[0].Action != "ledger.purchase" || audit[0].Actor != "test" {
			t.Fatalf("audit: %+v %v", audit, err)
		}
	})

	t.Run("overdraw and versions", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA, earnings)
		fund(t, s, walletA, 500)
		over := entry(ledger.KindTip, "tip_over", at,
			ledger.Posting{Account: walletA, Amount: 501}, ledger.Posting{Account: earnings, Amount: -501})
		if _, _, err := s.Post(ctx, over); !errors.Is(err, store.ErrInsufficientFunds) {
			t.Fatalf("overdraw: %v", err)
		}
		if _, err := s.FindEntry(ctx, tenant, "tip_over"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused entry was kept: %v", err)
		}
		b := balance(t, s, walletA)
		stale := entry(ledger.KindTip, "tip_stale", at,
			ledger.Posting{Account: walletA, Amount: 100}, ledger.Posting{Account: earnings, Amount: -100})
		stale.Expect = map[ledger.AccountID]int64{walletA: b.Version - 1}
		if _, _, err := s.Post(ctx, stale); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("stale version: %v", err)
		}
		stale.Expect = map[ledger.AccountID]int64{walletA: b.Version}
		if _, _, err := s.Post(ctx, stale); err != nil {
			t.Fatal(err)
		}
		if after := balance(t, s, walletA); after.Version != b.Version+1 || after.Amount != -400 {
			t.Fatalf("after: %+v", after)
		}
		exact := entry(ledger.KindTip, "tip_exact", at,
			ledger.Posting{Account: walletA, Amount: 400}, ledger.Posting{Account: earnings, Amount: -400})
		if _, _, err := s.Post(ctx, exact); err != nil {
			t.Fatalf("spending to exactly zero: %v", err)
		}
	})

	t.Run("reversal", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA)
		original := fund(t, s, walletA, 700)
		reversal := ledger.Reverse(original, id("je"), "rev:"+original.ID, "test", "refund", at.Add(time.Minute))
		if _, _, err := s.Post(ctx, reversal); err != nil {
			t.Fatal(err)
		}
		second := ledger.Reverse(original, id("je"), "rev2:"+original.ID, "test", "again", at)
		if _, _, err := s.Post(ctx, second); !errors.Is(err, store.ErrAlreadyReversed) {
			t.Fatalf("double reversal: %v", err)
		}
		found, err := s.FindReversal(ctx, tenant, original.ID)
		if err != nil || found.ID != reversal.ID || found.Reverses != original.ID {
			t.Fatalf("find reversal: %+v %v", found, err)
		}
		if b := balance(t, s, walletA); b.Amount != 0 {
			t.Fatalf("reversed balance: %+v", b)
		}
		ghost := ledger.Reverse(original, id("je"), "rev3", "test", "", at)
		ghost.Reverses = "je_missing"
		if _, _, err := s.Post(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("reversing nothing: %v", err)
		}
	})

	t.Run("credits since and listing", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA, walletB, earnings)
		fund(t, s, walletA, 10_000)
		for i, when := range []time.Time{at.Add(-10 * 24 * time.Hour), at.Add(-time.Hour), at} {
			e := entry(ledger.KindTip, fmt.Sprintf("tip_%d", i), when,
				ledger.Posting{Account: walletA, Amount: 100 * ledger.Amount(i+1)},
				ledger.Posting{Account: earnings, Amount: -100 * ledger.Amount(i+1)})
			if _, _, err := s.Post(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		recent, err := s.CreditsSince(ctx, earnings, at.Add(-7*24*time.Hour))
		if err != nil || recent != 500 {
			t.Fatalf("credits since: %d %v", recent, err)
		}
		tips, _ := s.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Kind: ledger.KindTip})
		if len(tips) != 3 || tips[0].IdempotencyKey != "tip_2" {
			t.Fatalf("newest first: %d %v", len(tips), tips)
		}
		limited, _ := s.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Account: walletA, Limit: 2})
		owned, _ := s.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Owner: "u_a"})
		other, _ := s.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Owner: "u_b"})
		if len(limited) != 2 || len(owned) != 4 || len(other) != 0 {
			t.Fatalf("filters: %d %d %d", len(limited), len(owned), len(other))
		}
	})

	t.Run("parallel spends never overdraw", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA, earnings)
		fund(t, s, walletA, 1000)
		var ok, refused atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _, err := s.Post(ctx, entry(ledger.KindTip, fmt.Sprintf("par_%d", i), at,
					ledger.Posting{Account: walletA, Amount: 70}, ledger.Posting{Account: earnings, Amount: -70}))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, store.ErrInsufficientFunds):
					refused.Add(1)
				default:
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		b := balance(t, s, walletA)
		if ok.Load() != 14 || refused.Load() != 26 || b.Amount != -20 {
			t.Fatalf("ok %d refused %d balance %d", ok.Load(), refused.Load(), b.Amount)
		}
	})

	t.Run("parallel replays post once", func(t *testing.T) {
		s := open(t)
		ensure(t, s, clearing, walletA)
		var created atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, fresh, err := s.Post(ctx, entry(ledger.KindPurchase, "pi_same", at,
					ledger.Posting{Account: clearing, Amount: 300}, ledger.Posting{Account: walletA, Amount: -300}))
				if err != nil {
					t.Error(err)
				}
				if fresh {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		if created.Load() != 1 || balance(t, s, walletA).Amount != -300 {
			t.Fatalf("created %d", created.Load())
		}
	})

	t.Run("random entries keep the books balanced", func(t *testing.T) {
		s := open(t)
		accounts := []ledger.AccountID{clearing, fees, tenantAc, walletA, walletB, earnings}
		ensure(t, s, accounts...)
		random := rand.New(rand.NewSource(7))
		expected := map[ledger.AccountID]ledger.Amount{}
		for i := 0; i < 150; i++ {
			from := accounts[random.Intn(len(accounts))]
			to := accounts[random.Intn(len(accounts))]
			if from == to {
				continue
			}
			amount := ledger.Amount(random.Intn(500) + 1)
			e := entry(ledger.KindAdjustment, fmt.Sprintf("rand_%d", i), at,
				ledger.Posting{Account: from, Amount: amount}, ledger.Posting{Account: to, Amount: -amount})
			_, _, err := s.Post(ctx, e)
			switch {
			case err == nil:
				expected[from] += amount
				expected[to] -= amount
			case errors.Is(err, store.ErrInsufficientFunds):
			default:
				t.Fatal(err)
			}
		}
		for _, a := range accounts {
			account, _ := s.GetAccount(ctx, a)
			b := balance(t, s, a)
			if b.Amount != expected[a] {
				t.Fatalf("%s: store %d, replayed %d", a, b.Amount, expected[a])
			}
			if !account.Kind.MayOverdraw() && account.Normal(b.Amount) < 0 {
				t.Fatalf("%s overdrawn: %d", a, b.Amount)
			}
		}
		trial, err := s.TrialBalance(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		for currency, sum := range trial {
			if sum != 0 {
				t.Fatalf("trial balance %s = %d", currency, sum)
			}
		}
	})

	t.Run("wallets", func(t *testing.T) {
		s := open(t)
		w := store.Wallet{Tenant: tenant, User: "u_a", CreatedAt: at, UpdatedAt: at}
		if err := s.SaveWallet(ctx, w); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveWallet(ctx, w); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("second create: %v", err)
		}
		got, err := s.GetWallet(ctx, tenant, "u_a")
		if err != nil || got.Version != 1 {
			t.Fatalf("wallet: %+v %v", got, err)
		}
		got.Frozen, got.FrozenReason = true, "chargeback"
		if err := s.SaveWallet(ctx, got); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetWallet(ctx, tenant, "u_a")
		if !got.Frozen || got.FrozenReason != "chargeback" || got.Version != 2 {
			t.Fatalf("frozen: %+v", got)
		}
		if _, err := s.GetWallet(ctx, tenant, "u_none"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing wallet: %v", err)
		}
	})

	t.Run("purchases", func(t *testing.T) {
		s := open(t)
		p := store.Purchase{ID: "pur_1", Tenant: tenant, User: "u_a", Pack: "p1", Currency: "usd", Price: 500,
			Credits: 550, Status: store.PurchasePending, Mode: "checkout", CreatedAt: at, UpdatedAt: at}
		if err := s.CreatePurchase(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetPurchase(ctx, tenant, "pur_1")
		got.StripeIntent, got.Status = "pi_9", store.PurchaseSucceeded
		if err := s.SavePurchase(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := s.SavePurchase(ctx, got); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("stale purchase: %v", err)
		}
		found, err := s.FindPurchaseByIntent(ctx, "pi_9")
		if err != nil || found.ID != "pur_1" || found.Credits != 550 || found.Status != store.PurchaseSucceeded {
			t.Fatalf("by intent: %+v %v", found, err)
		}
		if _, err := s.GetPurchase(ctx, "t_other", "pur_1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross tenant: %v", err)
		}
		dup := p
		dup.ID, dup.StripeIntent = "pur_2", "pi_9"
		if err := s.CreatePurchase(ctx, dup); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate intent: %v", err)
		}
		if n, _ := s.CountPurchases(ctx, tenant, "u_a", at.Add(-time.Minute)); n != 1 {
			t.Fatalf("count: %d", n)
		}
		if n, _ := s.CountPurchases(ctx, tenant, "u_a", at.Add(time.Minute)); n != 0 {
			t.Fatalf("count later: %d", n)
		}
		list, _ := s.ListPurchases(ctx, tenant, "u_a")
		if len(list) != 1 {
			t.Fatalf("list: %v", list)
		}
	})

	t.Run("tips", func(t *testing.T) {
		s := open(t)
		for i, tip := range []store.Tip{
			{Room: "r1", Sender: "u_a", Creator: "c_1", Amount: 100, Message: "hi", CreatedAt: at.Add(-2 * time.Hour)},
			{Room: "r1", Sender: "u_a", Creator: "c_1", Amount: 200, Message: "again", CreatedAt: at},
			{Room: "r2", Sender: "u_b", Creator: "c_1", Amount: 300, Message: "yo", CreatedAt: at},
		} {
			tip.ID, tip.Tenant, tip.Currency, tip.EntryID = fmt.Sprintf("tip_%d", i), tenant, "usd", fmt.Sprintf("je_%d", i)
			if err := s.CreateTip(ctx, tip); err != nil {
				t.Fatal(err)
			}
		}
		if n, sum, _ := s.TipStats(ctx, store.TipQuery{Tenant: tenant, Sender: "u_a", Since: at.Add(-time.Hour)}); n != 1 || sum != 200 {
			t.Fatalf("sender stats: %d %d", n, sum)
		}
		if n, sum, _ := s.TipStats(ctx, store.TipQuery{Tenant: tenant, Creator: "c_1"}); n != 3 || sum != 600 {
			t.Fatalf("creator stats: %d %d", n, sum)
		}
		room, _ := s.ListTips(ctx, store.TipQuery{Tenant: tenant, Room: "r1"})
		if len(room) != 2 || room[0].Message != "again" {
			t.Fatalf("room tips: %+v", room)
		}
		if n, _ := s.ScrubTips(ctx, tenant, "u_a"); n != 2 {
			t.Fatalf("scrubbed %d", n)
		}
		room, _ = s.ListTips(ctx, store.TipQuery{Tenant: tenant, Sender: "u_a"})
		if room[0].Message != "" || room[0].Amount != 200 {
			t.Fatalf("scrub kept the message or lost the amount: %+v", room[0])
		}
	})

	t.Run("payees and payouts", func(t *testing.T) {
		s := open(t)
		p := store.Payee{Tenant: tenant, Creator: "c_1", StripeAccount: "acct_1", KYC: store.KYCPending, CreatedAt: at, UpdatedAt: at}
		if err := s.SavePayee(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, _ := s.FindPayeeByStripeAccount(ctx, "acct_1")
		got.KYC, got.PayoutsEnabled = store.KYCVerified, true
		if err := s.SavePayee(ctx, got); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetPayee(ctx, tenant, "c_1")
		if !got.Payable() || got.Version != 2 {
			t.Fatalf("payee: %+v", got)
		}
		other := store.Payee{Tenant: tenant, Creator: "c_2", StripeAccount: "acct_1", CreatedAt: at, UpdatedAt: at}
		if err := s.SavePayee(ctx, other); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("shared connected account: %v", err)
		}
		po := store.Payout{ID: "po_1", Tenant: tenant, Creator: "c_1", Currency: "usd", Amount: 900,
			Status: store.PayoutPending, Run: "2026-10-06", EntryID: "je_1", CreatedAt: at, UpdatedAt: at}
		if err := s.CreatePayout(ctx, po); err != nil {
			t.Fatal(err)
		}
		if err := s.CreatePayout(ctx, po); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate payout: %v", err)
		}
		po, _ = s.GetPayout(ctx, tenant, "po_1")
		po.StripeTransfer, po.Status = "tr_1", store.PayoutPaid
		if err := s.SavePayout(ctx, po); err != nil {
			t.Fatal(err)
		}
		found, err := s.FindPayoutByTransfer(ctx, "tr_1")
		if err != nil || found.ID != "po_1" || found.Status != store.PayoutPaid {
			t.Fatalf("by transfer: %+v %v", found, err)
		}
		if list, _ := s.ListPayouts(ctx, store.PayoutQuery{Tenant: tenant, Status: store.PayoutPending}); len(list) != 0 {
			t.Fatalf("pending: %v", list)
		}
		if list, _ := s.ListPayouts(ctx, store.PayoutQuery{Tenant: tenant, Creator: "c_1"}); len(list) != 1 {
			t.Fatalf("by creator: %v", list)
		}
	})

	t.Run("reviews", func(t *testing.T) {
		s := open(t)
		r := store.Review{ID: "rv_1", Tenant: tenant, Subject: "wallet:u_a", Reason: "chargeback", Status: store.ReviewOpen, CreatedAt: at, UpdatedAt: at}
		if err := s.CreateReview(ctx, r); err != nil {
			t.Fatal(err)
		}
		open, _ := s.ListReviews(ctx, tenant, store.ReviewOpen, "wallet:u_a")
		if len(open) != 1 {
			t.Fatalf("open: %v", open)
		}
		r, _ = s.GetReview(ctx, tenant, "rv_1")
		r.Status, r.ResolvedBy = store.ReviewApproved, "k1"
		if err := s.SaveReview(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveReview(ctx, r); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("stale review: %v", err)
		}
		if open, _ := s.ListReviews(ctx, tenant, store.ReviewOpen, ""); len(open) != 0 {
			t.Fatalf("still open: %v", open)
		}
	})

	t.Run("webhooks and audit", func(t *testing.T) {
		s := open(t)
		if fresh, err := s.RecordWebhook(ctx, "evt_1", "payment_intent.succeeded", at); err != nil || !fresh {
			t.Fatalf("first: %v %v", fresh, err)
		}
		if fresh, _ := s.RecordWebhook(ctx, "evt_1", "payment_intent.succeeded", at); fresh {
			t.Fatal("replay reported new")
		}
		if err := s.ForgetWebhook(ctx, "evt_1"); err != nil {
			t.Fatal(err)
		}
		if fresh, _ := s.RecordWebhook(ctx, "evt_1", "payment_intent.succeeded", at); !fresh {
			t.Fatal("forgotten event not new")
		}
		for i := 0; i < 3; i++ {
			if err := s.AppendAudit(ctx, store.AuditRecord{ID: fmt.Sprintf("au_x%d", i), Tenant: tenant, Actor: "k1",
				Action: "wallet.freeze", Subject: "wallet:u_a", At: at.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AppendAudit(ctx, store.AuditRecord{ID: "au_x0", Tenant: tenant, At: at}); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate audit: %v", err)
		}
		list, err := s.ListAudit(ctx, tenant, "wallet:u_a", 2)
		if err != nil || len(list) != 2 || list[0].ID != "au_x2" {
			t.Fatalf("audit: %+v %v", list, err)
		}
	})
}
