// Copyright 2026 Elliott Wave LLC. All rights reserved.

package ledger

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestAccountIDRoundTrip(t *testing.T) {
	cases := []struct {
		kind  AccountKind
		owner string
	}{{Wallet, "u_1"}, {Earnings, "c_1"}, {PendingPayout, "c_1"}, {Fees, ""}, {Tenant, ""}, {Platform, ""}, {StripeClearing, ""}}
	for _, c := range cases {
		id := AccountFor("t_1", c.kind, c.owner, "usd")
		tenant, kind, owner, currency, err := id.Parse()
		if err != nil || tenant != "t_1" || kind != c.kind || owner != c.owner || currency != "usd" {
			t.Fatalf("%s: %s %s %q %s %v", id, tenant, kind, owner, currency, err)
		}
	}
	for _, bad := range []AccountID{"t_1/wallet/-/usd", "t_1/fees/u_1/usd", "t_1/wallet/u_1/USD", "t_1/nope/-/usd", "a/b/c", "t 1/fees/-/usd"} {
		if _, _, _, _, err := bad.Parse(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s parsed: %v", bad, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	wallet := AccountFor("t_1", Wallet, "u_1", "usd")
	clearing := AccountFor("t_1", StripeClearing, "", "usd")
	base := func() Entry {
		return Entry{ID: "je_1", Tenant: "t_1", Kind: KindPurchase, IdempotencyKey: "k",
			Postings: []Posting{{clearing, 100}, {wallet, -100}}}
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Entry){
		"unbalanced":     func(e *Entry) { e.Postings[0].Amount = 101 },
		"zero":           func(e *Entry) { e.Postings = []Posting{{clearing, 0}, {wallet, 0}} },
		"one posting":    func(e *Entry) { e.Postings = e.Postings[:1] },
		"no key":         func(e *Entry) { e.IdempotencyKey = "" },
		"other tenant":   func(e *Entry) { e.Postings[1].Account = AccountFor("t_2", Wallet, "u_1", "usd") },
		"duplicate":      func(e *Entry) { e.Postings = []Posting{{wallet, 100}, {wallet, -100}} },
		"too large":      func(e *Entry) { e.Postings = []Posting{{clearing, MaxAmount + 1}, {wallet, -MaxAmount - 1}} },
		"mixed currency": func(e *Entry) { e.Postings[1].Account = AccountFor("t_1", Wallet, "u_1", "eur") },
	}
	for name, mutate := range cases {
		entry := base()
		mutate(&entry)
		if err := entry.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// Property: any entry built from random balanced postings validates, its
// reversal validates, and the two together leave every account at zero.
func TestRandomEntriesBalanceAndReverse(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	accounts := []AccountID{
		AccountFor("t_1", StripeClearing, "", "usd"), AccountFor("t_1", Fees, "", "usd"),
		AccountFor("t_1", Tenant, "", "usd"), AccountFor("t_1", Platform, "", "usd"),
		AccountFor("t_1", Wallet, "u_1", "usd"), AccountFor("t_1", Wallet, "u_2", "usd"),
		AccountFor("t_1", Earnings, "c_1", "usd"), AccountFor("t_1", PendingPayout, "c_1", "usd"),
		AccountFor("t_1", Wallet, "u_1", "eur"), AccountFor("t_1", StripeClearing, "", "eur"),
	}
	for i := 0; i < 2000; i++ {
		entry := randomEntry(random, accounts)
		if err := entry.Validate(); err != nil {
			t.Fatalf("random entry %d: %v: %+v", i, err, entry.Postings)
		}
		reversal := Reverse(entry, "je_r", "r", "test", "undo", at)
		if err := reversal.Validate(); err != nil {
			t.Fatal(err)
		}
		net := map[AccountID]Amount{}
		for _, p := range append(entry.Postings, reversal.Postings...) {
			net[p.Account] += p.Amount
		}
		for id, sum := range net {
			if sum != 0 {
				t.Fatalf("%s nets to %d", id, sum)
			}
		}
		if entry.Fingerprint() == reversal.Fingerprint() {
			t.Fatal("a reversal has the fingerprint of its original")
		}
	}
}

// randomEntry picks distinct accounts in one currency and balances them.
func randomEntry(random *rand.Rand, accounts []AccountID) Entry {
	currency := Currency("usd")
	if random.Intn(5) == 0 {
		currency = "eur"
	}
	var pool []AccountID
	for _, id := range accounts {
		if _, _, _, c, _ := id.Parse(); c == currency {
			pool = append(pool, id)
		}
	}
	random.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	n := 2 + random.Intn(len(pool)-1)
	postings := make([]Posting, 0, n)
	var sum Amount
	for _, id := range pool[:n-1] {
		amount := Amount(random.Int63n(1_000_000) + 1)
		if random.Intn(2) == 0 {
			amount = -amount
		}
		postings = append(postings, Posting{id, amount})
		sum += amount
	}
	if sum == 0 {
		postings[0].Amount++
		sum++
	}
	postings = append(postings, Posting{pool[n-1], -sum})
	return Entry{ID: "je", Tenant: "t_1", Kind: KindAdjustment, IdempotencyKey: "k", Postings: postings}
}

// Property: the parts of a split always sum to the whole, fees round down,
// and the remainder is never negative for rates summing to at most 100%.
func TestSplitConserves(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	for i := 0; i < 10000; i++ {
		amount := Amount(random.Int63n(int64(MaxAmount)) + 1)
		a := BasisPoints(random.Intn(10_001))
		b := BasisPoints(random.Intn(10_001 - int(a)))
		fees, remainder := Split(amount, a, b)
		if fees[0]+fees[1]+remainder != amount || remainder < 0 {
			t.Fatalf("split %d at %d+%d: %v %d", amount, a, b, fees, remainder)
		}
		if fees[0]*10_000 > amount*Amount(a) {
			t.Fatalf("fee rounded up: %d of %d at %d", fees[0], amount, a)
		}
	}
	if fees, rest := Split(1, 500, 500); fees[0] != 0 || fees[1] != 0 || rest != 1 {
		t.Fatalf("one unit: %v %d", fees, rest)
	}
}

func TestNormalBalance(t *testing.T) {
	wallet, _ := NewAccount(AccountFor("t_1", Wallet, "u_1", "usd"), at)
	clearing, _ := NewAccount(AccountFor("t_1", StripeClearing, "", "usd"), at)
	if wallet.Normal(-500) != 500 || clearing.Normal(500) != 500 {
		t.Fatal("normal balance sign")
	}
}
