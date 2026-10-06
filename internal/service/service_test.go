// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service_test

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

func TestPurchaseCreditsTheWalletOnce(t *testing.T) {
	h := newHarness(t)
	p, intent := h.buy("u_1", "big", "buy-1")
	if p.Status != store.PurchaseSucceeded || p.StripeIntent != intent || p.EntryID == "" {
		t.Fatalf("purchase: %+v", p)
	}
	if h.wallet("u_1") != 1100 || h.balance(ledger.StripeClearing, "") != 1000 || h.balance(ledger.Tenant, "") != -100 {
		t.Fatalf("wallet %d clearing %d tenant %d", h.wallet("u_1"), h.balance(ledger.StripeClearing, ""), h.balance(ledger.Tenant, ""))
	}
	// The same request again finds the same purchase and the same session.
	again, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "big", Mode: service.ModeCheckout,
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/cancel", IdempotencyKey: "buy-1"})
	if err != nil || again.Purchase.ID != p.ID || again.Purchase.StripeCheckout != p.StripeCheckout {
		t.Fatalf("replay: %+v %v", again, err)
	}
	// The same key for a different pack is refused, not silently merged.
	if _, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "small", Mode: service.ModeCheckout,
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/cancel", IdempotencyKey: "buy-1"}); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("key reuse: %v", err)
	}
	// Stripe delivering the success twice, under different event IDs, still
	// credits once: the ledger key is the payment intent.
	h.deliver(h.fake.SucceedIntent(intent, 1000))
	if h.wallet("u_1") != 1100 {
		t.Fatalf("double credit: %d", h.wallet("u_1"))
	}
}

func TestPurchaseValidation(t *testing.T) {
	h := newHarness(t)
	cases := map[string]service.PurchaseRequest{
		"unknown pack": {User: "u_1", Pack: "nope", Mode: service.ModeCheckout, SuccessURL: "https://a.example", CancelURL: "https://a.example", IdempotencyKey: "k1"},
		"http return":  {User: "u_1", Pack: "small", Mode: service.ModeCheckout, SuccessURL: "http://a.example", CancelURL: "https://a.example", IdempotencyKey: "k2"},
		"bad mode":     {User: "u_1", Pack: "small", Mode: "cash", IdempotencyKey: "k3"},
		"no key":       {User: "u_1", Pack: "small", Mode: service.ModePaymentIntent},
		"bad user":     {User: "u 1", Pack: "small", Mode: service.ModePaymentIntent, IdempotencyKey: "k4"},
	}
	for name, r := range cases {
		if _, err := h.svc.Purchase(ctx, "key:test", tenant, r); !errors.Is(err, service.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPaymentIntentMismatchIsFlaggedNotCredited(t *testing.T) {
	h := newHarness(t)
	result, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "big", Mode: service.ModePaymentIntent, IdempotencyKey: "pi-1"})
	if err != nil || result.ClientSecret == "" || result.Purchase.StripeIntent == "" {
		t.Fatalf("intent: %+v %v", result, err)
	}
	h.deliver(h.fake.SucceedIntent(result.Purchase.StripeIntent, 999))
	p, _ := h.svc.GetPurchase(ctx, tenant, result.Purchase.ID)
	if p.Status != store.PurchaseFlagged || h.wallet("u_1") != 0 || len(h.openReviews("purchase:"+p.ID)) != 1 {
		t.Fatalf("mismatch credited or not flagged: %+v wallet %d", p, h.wallet("u_1"))
	}
}

func TestPurchaseVelocity(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) { c.PurchasesPerHour = 2 })
	for i := 0; i < 2; i++ {
		h.buy("u_1", "small", fmt.Sprintf("v-%d", i))
	}
	_, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "small", Mode: service.ModePaymentIntent, IdempotencyKey: "v-2"})
	if !errors.Is(err, service.ErrRateLimited) {
		t.Fatalf("third purchase in an hour: %v", err)
	}
	h.clock.Advance(time.Hour + time.Second)
	if _, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "small", Mode: service.ModePaymentIntent, IdempotencyKey: "v-2"}); err != nil {
		t.Fatalf("after an hour: %v", err)
	}
}

func TestTipSplitsFeesAndBroadcasts(t *testing.T) {
	h := newHarness(t)
	h.buy("u_1", "big", "buy")
	result, err := h.tip("u_1", "c_1", 1000, "tip-1")
	if err != nil {
		t.Fatal(err)
	}
	if h.wallet("u_1") != 100 || h.balance(ledger.Earnings, "c_1") != 850 || h.balance(ledger.Fees, "") != 50 ||
		h.balance(ledger.Tenant, "") != -100+100 {
		t.Fatalf("split: wallet %d creator %d fees %d tenant %d", h.wallet("u_1"), h.balance(ledger.Earnings, "c_1"),
			h.balance(ledger.Fees, ""), h.balance(ledger.Tenant, ""))
	}
	sent := h.broadcasts()
	if len(sent) != 1 || sent[0].Type != "motom.tip.v1" || sent[0].Amount != 1000 || sent[0].From != "u_1" ||
		sent[0].To != "c_1" || sent[0].Room != "room_1" || sent[0].ID != result.Tip.ID || sent[0].Validate() != nil {
		t.Fatalf("broadcast: %+v", sent)
	}
	replay, err := h.tip("u_1", "c_1", 1000, "tip-1")
	if err != nil || !replay.Replayed || replay.Tip.ID != result.Tip.ID || replay.Entry.ID != result.Entry.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if len(h.broadcasts()) != 1 || h.wallet("u_1") != 100 {
		t.Fatal("a replayed tip moved money or broadcast again")
	}
	if _, err := h.tip("u_1", "c_1", 200, "tip-2"); !errors.Is(err, service.ErrInsufficientFunds) {
		t.Fatalf("overdraw: %v", err)
	}
	if _, err := h.tip("u_1", "u_1", 60, "tip-3"); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("self tip: %v", err)
	}
	if _, err := h.tip("u_1", "c_1", 10, "tip-4"); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("below minimum: %v", err)
	}
	tips, _ := h.svc.Tips(ctx, tenant, store.TipQuery{Room: "room_1"})
	if len(tips) != 1 || tips[0].Message != "nice" {
		t.Fatalf("room tips: %+v", tips)
	}
}

func TestTipRateLimitsAndDailyCaps(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) {
		c.SenderTipsPerMin, c.SenderDailyCap, c.RoomTipsPerMin = 3, 1000, 100
	})
	h.grant("u_1", 5000, "g")
	for i := 0; i < 3; i++ {
		if _, err := h.tip("u_1", "c_1", 100, fmt.Sprintf("r-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.tip("u_1", "c_1", 100, "r-3"); !errors.Is(err, service.ErrRateLimited) {
		t.Fatalf("fourth tip in a minute: %v", err)
	}
	h.clock.Advance(time.Minute + time.Second)
	if _, err := h.tip("u_1", "c_1", 600, "r-4"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tip("u_1", "c_1", 200, "r-5"); !errors.Is(err, service.ErrLimitExceeded) {
		t.Fatalf("over the daily cap: %v", err)
	}
	h.clock.Advance(24*time.Hour + time.Second)
	if _, err := h.tip("u_1", "c_1", 200, "r-5"); err != nil {
		t.Fatalf("a day later: %v", err)
	}
	audit, _ := h.svc.Audit(ctx, tenant, "wallet:u_1", 0)
	limited := 0
	for _, a := range audit {
		if a.Action == "fraud.tip_limit" {
			limited++
		}
	}
	if limited != 2 {
		t.Fatalf("limit hits audited: %d", limited)
	}
}

func TestRoomCap(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) { c.RoomDailyCap = 300 })
	h.grant("u_1", 1000, "g1")
	h.grant("u_2", 1000, "g2")
	if _, err := h.tip("u_1", "c_1", 200, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tip("u_2", "c_1", 200, "b"); !errors.Is(err, service.ErrLimitExceeded) {
		t.Fatalf("room cap: %v", err)
	}
}

// Many parallel tips from one wallet: exactly as many succeed as the wallet
// can pay for, and it never goes below zero.
func TestParallelTipsNeverOverdraw(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) {
		c.SenderTipsPerMin, c.SenderDailyCap, c.RoomTipsPerMin, c.RepeatPairPerHour = 1000, 1_000_000, 1000, 1000
	})
	h.grant("u_1", 1000, "g")
	var ok, broke atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.tip("u_1", fmt.Sprintf("c_%d", i%4), 100, fmt.Sprintf("p-%d", i))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, service.ErrInsufficientFunds):
				broke.Add(1)
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 10 || broke.Load() != 50 || h.wallet("u_1") != 0 {
		t.Fatalf("ok %d refused %d wallet %d", ok.Load(), broke.Load(), h.wallet("u_1"))
	}
	var earned ledger.Amount
	for i := 0; i < 4; i++ {
		earned += h.balance(ledger.Earnings, fmt.Sprintf("c_%d", i))
	}
	if earned != 10*85 || len(h.broadcasts()) != 10 {
		t.Fatalf("earned %d, broadcasts %d", earned, len(h.broadcasts()))
	}
}

// Parallel tips and spends from several wallets at once, through the
// optimistic-concurrency retry path.
func TestParallelMixedTraffic(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) {
		c.SenderTipsPerMin, c.SenderDailyCap, c.RoomTipsPerMin, c.RepeatPairPerHour = 1000, 1_000_000, 1000, 1000
	})
	for u := 0; u < 3; u++ {
		h.grant(fmt.Sprintf("u_%d", u), 2000, fmt.Sprintf("g%d", u))
	}
	var wg sync.WaitGroup
	for i := 0; i < 90; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("u_%d", i%3)
			var err error
			if i%2 == 0 {
				_, err = h.tip(user, "c_1", 150, fmt.Sprintf("m-%d", i))
			} else {
				_, err = h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: user, Amount: 120, IdempotencyKey: fmt.Sprintf("s-%d", i)})
			}
			if err != nil && !errors.Is(err, service.ErrInsufficientFunds) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

func TestSpendAndGrant(t *testing.T) {
	h := newHarness(t)
	h.grant("u_1", 300, "promo")
	h.grant("u_1", 300, "promo")
	if h.wallet("u_1") != 300 || h.balance(ledger.Tenant, "") != -300 {
		t.Fatalf("grant: %d %d", h.wallet("u_1"), h.balance(ledger.Tenant, ""))
	}
	e, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 200, Reference: "sticker", IdempotencyKey: "s1"})
	if err != nil || e.Kind != ledger.KindSpend {
		t.Fatal(err)
	}
	// 5% platform fee on 200 is 10; the tenant keeps 190.
	if h.wallet("u_1") != 100 || h.balance(ledger.Fees, "") != 10 || h.balance(ledger.Tenant, "") != -300+190 {
		t.Fatalf("spend: %d %d %d", h.wallet("u_1"), h.balance(ledger.Fees, ""), h.balance(ledger.Tenant, ""))
	}
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 101, IdempotencyKey: "s2"}); !errors.Is(err, service.ErrInsufficientFunds) {
		t.Fatalf("overspend: %v", err)
	}
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 100, Currency: "eur", IdempotencyKey: "s3"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("currency not enabled: %v", err)
	}
	if _, err := h.svc.SetFrozen(ctx, "key:test", tenant, "u_1", true, "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 10, IdempotencyKey: "s4"}); !errors.Is(err, service.ErrFrozen) {
		t.Fatalf("frozen spend: %v", err)
	}
	// A replay of a spend made before the freeze still answers.
	if again, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 200, IdempotencyKey: "s1"}); err != nil || again.ID != e.ID {
		t.Fatalf("replay after freeze: %v", err)
	}
}

func TestReusedKeyForADifferentRequestIsRefused(t *testing.T) {
	h := newHarness(t)
	h.grant("u_1", 1000, "g1")
	if _, err := h.svc.Grant(ctx, "key:test", tenant, service.GrantRequest{User: "u_1", Amount: 999, IdempotencyKey: "g1"}); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("grant: %v", err)
	}
	if _, err := h.svc.Grant(ctx, "key:test", tenant, service.GrantRequest{User: "u_2", Amount: 1000, IdempotencyKey: "g1"}); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("grant to another user: %v", err)
	}
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 100, IdempotencyKey: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 100, Currency: "eur", IdempotencyKey: "s1"}); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("spend in another currency: %v", err)
	}
	if _, err := h.tip("u_1", "c_1", 100, "t1"); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]service.TipRequest{
		"amount":  {Room: "room_1", Sender: "u_1", Creator: "c_1", Amount: 200, IdempotencyKey: "t1"},
		"creator": {Room: "room_1", Sender: "u_1", Creator: "c_2", Amount: 100, IdempotencyKey: "t1"},
		"room":    {Room: "room_2", Sender: "u_1", Creator: "c_1", Amount: 100, IdempotencyKey: "t1"},
		"sender":  {Room: "room_1", Sender: "u_2", Creator: "c_1", Amount: 100, IdempotencyKey: "t1"},
	} {
		if _, err := h.svc.Tip(ctx, "key:test", tenant, r); !errors.Is(err, store.ErrIdempotencyMismatch) {
			t.Errorf("tip with another %s: %v", name, err)
		}
	}
	if h.wallet("u_1") != 800 {
		t.Fatalf("wallet %d", h.wallet("u_1"))
	}
	p, _ := h.buy("u_3", "big", "buy")
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 300, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 400, "r1"); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("refund of another amount: %v", err)
	}
}

func TestRefundPartialThenRest(t *testing.T) {
	h := newHarness(t)
	p, _ := h.buy("u_1", "big", "buy")
	refunded, e, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 500, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Half the price removes half the credits (550), and half the bonus goes
	// back to the tenant that funded it.
	if refunded.Refunded != 500 || h.wallet("u_1") != 550 || h.balance(ledger.StripeClearing, "") != 500 || h.balance(ledger.Tenant, "") != -50 {
		t.Fatalf("partial: %+v wallet %d clearing %d tenant %d", refunded, h.wallet("u_1"), h.balance(ledger.StripeClearing, ""), h.balance(ledger.Tenant, ""))
	}
	again, e2, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 500, "r1")
	if err != nil || e2.ID != e.ID || again.Refunded != 500 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if n := len(h.fake.Refunds()); n != 1 {
		t.Fatalf("stripe refunds after a replay: %d", n)
	}
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 600, "r2"); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("over-refund: %v", err)
	}
	rest, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r3")
	if err != nil || rest.Status != store.PurchaseRefunded || rest.Refunded != 1000 {
		t.Fatalf("rest: %+v %v", rest, err)
	}
	if h.wallet("u_1") != 0 || h.balance(ledger.StripeClearing, "") != 0 || h.balance(ledger.Tenant, "") != 0 {
		t.Fatal("a full refund did not return everything to zero")
	}
	// Stripe's charge.refunded for refunds MOTOM made changes nothing.
	h.deliver(h.fake.RefundOutside(p.StripeIntent, 1000))
	if h.wallet("u_1") != 0 || len(h.openReviews("purchase:"+p.ID)) != 0 {
		t.Fatal("our own refund was treated as an outside one")
	}
}

func TestRefundOfSpentCreditsIsRefused(t *testing.T) {
	h := newHarness(t)
	p, _ := h.buy("u_1", "small", "buy")
	if _, err := h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: "u_1", Amount: 400, IdempotencyKey: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r"); !errors.Is(err, service.ErrInsufficientFunds) {
		t.Fatalf("refunding spent credit: %v", err)
	}
	after, _ := h.svc.GetPurchase(ctx, tenant, p.ID)
	if after.Refunded != 0 || after.Status != store.PurchaseSucceeded || len(h.fake.Refunds()) != 0 {
		t.Fatalf("reservation not released: %+v", after)
	}
}

func TestRefundStripeRefusesUnwinds(t *testing.T) {
	h := newHarness(t)
	p, _ := h.buy("u_1", "big", "buy")
	h.fake.Fail("/v1/refunds", http.StatusBadRequest, "charge_already_refunded")
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r"); !errors.Is(err, service.ErrStripeRefused) {
		t.Fatalf("refused refund: %v", err)
	}
	after, _ := h.svc.GetPurchase(ctx, tenant, p.ID)
	if h.wallet("u_1") != 1100 || after.Refunded != 0 || after.Status != store.PurchaseSucceeded {
		t.Fatalf("not unwound: wallet %d %+v", h.wallet("u_1"), after)
	}
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r"); !errors.Is(err, service.ErrStripeRefused) {
		t.Fatalf("replay of a refused refund: %v", err)
	}
	// An unknown outcome leaves the entry standing; the retry completes it
	// under the same Stripe idempotency key.
	h.fake.Fail("/v1/refunds", http.StatusServiceUnavailable, "api_error")
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r-2"); err == nil || stripe.Definitive(err) {
		t.Fatalf("unknown outcome: %v", err)
	}
	if h.wallet("u_1") != 0 {
		t.Fatalf("entry should stand: %d", h.wallet("u_1"))
	}
	if _, _, err := h.svc.Refund(ctx, "key:test", tenant, p.ID, 0, "r-2"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.fake.Refunds()); n != 1 {
		t.Fatalf("stripe refunds: %d", n)
	}
}

func TestRefundOutsideMOTOM(t *testing.T) {
	h := newHarness(t)
	p, intent := h.buy("u_1", "big", "buy")
	h.deliver(h.fake.RefundOutside(intent, 400))
	after, _ := h.svc.GetPurchase(ctx, tenant, p.ID)
	if after.Refunded != 400 || h.wallet("u_1") != 1100-440 || len(h.openReviews("purchase:"+p.ID)) != 1 {
		t.Fatalf("outside refund: %+v wallet %d", after, h.wallet("u_1"))
	}
}

func TestChargebackReversesAndFreezes(t *testing.T) {
	for _, liability := range []store.Liability{store.LiabilityPlatform, store.LiabilityTenant} {
		t.Run(string(liability), func(t *testing.T) {
			h := newHarness(t, func(c *store.TenantConfig) { c.ChargebackLiability = liability })
			p, intent := h.buy("u_1", "big", "buy")
			if _, err := h.tip("u_1", "c_1", 600, "tip"); err != nil {
				t.Fatal(err)
			}
			tenantBefore := h.balance(ledger.Tenant, "")
			delivery, dispute := h.fake.Dispute(intent, 1000)
			h.deliver(delivery)
			// The wallet had 500 of the 1100 credits left; the 600 it tipped
			// away is the loss.
			if h.wallet("u_1") != 0 || h.balance(ledger.StripeClearing, "") != 0 {
				t.Fatalf("chargeback: wallet %d clearing %d", h.wallet("u_1"), h.balance(ledger.StripeClearing, ""))
			}
			switch liability {
			case store.LiabilityPlatform:
				if h.balance(ledger.Platform, "") != 600 || h.balance(ledger.Tenant, "") != tenantBefore+100 {
					t.Fatalf("platform loss %d tenant %d", h.balance(ledger.Platform, ""), h.balance(ledger.Tenant, ""))
				}
			case store.LiabilityTenant:
				if h.balance(ledger.Platform, "") != 0 || h.balance(ledger.Tenant, "") != tenantBefore+100-600 {
					t.Fatalf("tenant loss: %d", h.balance(ledger.Tenant, ""))
				}
			}
			after, _ := h.svc.GetPurchase(ctx, tenant, p.ID)
			state, _ := h.svc.Wallet(ctx, tenant, "u_1")
			if after.Status != store.PurchaseDisputed || !state.Wallet.Frozen || len(h.openReviews("wallet:u_1")) != 1 {
				t.Fatalf("not frozen and queued: %+v %+v", after, state.Wallet)
			}
			h.grant("u_1", 100, "g")
			if _, err := h.tip("u_1", "c_1", 60, "tip-2"); !errors.Is(err, service.ErrFrozen) {
				t.Fatalf("frozen tip: %v", err)
			}
			// A redelivery of the dispute, under a new event ID, posts nothing.
			again, _ := h.fake.Dispute(intent, 1000)
			_ = again
			h.deliver(h.fake.CloseDispute(intent, dispute, 1000, "won"))
			if h.wallet("u_1") != 600 || h.balance(ledger.StripeClearing, "") != 1000 || h.balance(ledger.Platform, "") != 0 {
				t.Fatalf("won: wallet %d clearing %d platform %d", h.wallet("u_1"), h.balance(ledger.StripeClearing, ""), h.balance(ledger.Platform, ""))
			}
			h.deliver(h.fake.CloseDispute(intent, dispute, 1000, "won"))
			if h.wallet("u_1") != 600 {
				t.Fatal("dispute won twice")
			}
			state, _ = h.svc.Wallet(ctx, tenant, "u_1")
			if !state.Wallet.Frozen {
				t.Fatal("winning the dispute unfroze the wallet without a review")
			}
			review := h.openReviews("wallet:u_1")[0]
			if _, err := h.svc.ResolveReview(ctx, "key:reviewer", tenant, review.ID, true, "card owner confirmed"); err != nil {
				t.Fatal(err)
			}
			state, _ = h.svc.Wallet(ctx, tenant, "u_1")
			if state.Wallet.Frozen {
				t.Fatal("approved review did not unfreeze")
			}
		})
	}
}

func TestDuplicateDisputeDeliveryPostsOnce(t *testing.T) {
	h := newHarness(t)
	_, intent := h.buy("u_1", "big", "buy")
	delivery, _ := h.fake.Dispute(intent, 1000)
	h.deliver(delivery)
	h.deliver(delivery)
	chargebacks, _ := h.store.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Kind: ledger.KindChargeback})
	if len(chargebacks) != 1 || h.balance(ledger.StripeClearing, "") != 0 {
		t.Fatalf("chargebacks %d", len(chargebacks))
	}
}

func TestWebhookSignatureAndReplay(t *testing.T) {
	h := newHarness(t)
	result, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: "u_1", Pack: "small", Mode: service.ModePaymentIntent, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	d := h.fake.SucceedIntent(result.Purchase.StripeIntent, 500)
	if err := h.svc.HandleWebhook(ctx, d.Payload, "t=1,v1=00"); !errors.Is(err, service.ErrSignature) {
		t.Fatalf("bad signature: %v", err)
	}
	if err := h.svc.HandleWebhook(ctx, append(d.Payload, ' '), d.Header); !errors.Is(err, service.ErrSignature) {
		t.Fatalf("altered body: %v", err)
	}
	h.clock.Advance(10 * time.Minute)
	if err := h.svc.HandleWebhook(ctx, d.Payload, d.Header); !errors.Is(err, service.ErrSignature) {
		t.Fatalf("stale delivery: %v", err)
	}
	h.clock.Advance(-10 * time.Minute)
	h.deliver(d)
	h.deliver(d)
	if h.wallet("u_1") != 500 {
		t.Fatalf("wallet %d", h.wallet("u_1"))
	}
	// A failing handler forgets the event so Stripe's retry is processed.
	broken := h.fake.Raw("payment_intent.succeeded", "not an object")
	if err := h.svc.HandleWebhook(ctx, broken.Payload, broken.Header); err == nil {
		t.Fatal("malformed object accepted")
	}
	if fresh, _ := h.store.RecordWebhook(ctx, broken.EventID, "x", h.clock.Now()); !fresh {
		t.Fatal("failed event was remembered")
	}
	// Events that are not MOTOM's (MOQOM billing on the same account) are
	// acknowledged and ignored.
	foreign := h.fake.Raw("payment_intent.succeeded", stripe.PaymentIntent{ID: "pi_other", AmountReceived: 100, Currency: "usd",
		Metadata: map[string]string{"tenant": "moqom", "purpose": "top_up"}})
	h.deliver(foreign)
}

func TestPayoutGatingHoldAndMinimum(t *testing.T) {
	h := newHarness(t, func(c *store.TenantConfig) { c.HoldDays, c.MinPayout, c.ReviewFirstPayoutAt = 7, 500, 1_000_000 })
	h.buy("u_1", "big", "buy")
	h.clock.Advance(2 * time.Hour)
	if _, err := h.tip("u_1", "c_1", 1000, "tip"); err != nil {
		t.Fatal(err)
	}
	report, err := h.svc.RunPayouts(ctx, "scheduler", tenant, "run-1")
	if err != nil || len(report.Paid) != 0 {
		t.Fatalf("report: %+v %v", report, err)
	}
	h.svc.Onboard(ctx, "key:test", tenant, "c_1", "US", "https://app.example/refresh", "https://app.example/done")
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "run-2")
	if report.Skipped["kyc_pending"] != 1 {
		t.Fatalf("pending KYC paid: %+v", report)
	}
	payee, _ := h.store.GetPayee(ctx, tenant, "c_1")
	h.deliver(h.fake.UpdateAccount(payee.StripeAccount, false, true, "requirements.past_due"))
	payee, _ = h.store.GetPayee(ctx, tenant, "c_1")
	if payee.KYC != store.KYCRestricted {
		t.Fatalf("restricted: %+v", payee)
	}
	h.deliver(h.fake.UpdateAccount(payee.StripeAccount, true, true, ""))
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "run-3")
	if report.Skipped["below_minimum"] != 1 || len(h.fake.Transfers()) != 0 {
		t.Fatalf("held earnings paid: %+v", report)
	}
	state, _ := h.svc.Payee(ctx, tenant, "c_1")
	if state.Earnings[0].Held != 850 || state.Earnings[0].Available != 0 {
		t.Fatalf("held: %+v", state.Earnings)
	}
	h.clock.Advance(8 * 24 * time.Hour)
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "run-4")
	if len(report.Paid) != 1 {
		t.Fatalf("not paid after the hold: %+v", report)
	}
	transfers := h.fake.Transfers()
	if len(transfers) != 1 {
		t.Fatalf("transfers: %v", transfers)
	}
	for _, tr := range transfers {
		if tr.Amount != 850 || tr.Destination != payee.StripeAccount || tr.Currency != "usd" {
			t.Fatalf("transfer: %+v", tr)
		}
	}
	if h.balance(ledger.Earnings, "c_1") != 0 || h.balance(ledger.PendingPayout, "c_1") != 0 || h.balance(ledger.StripeClearing, "") != 150 {
		t.Fatalf("after payout: earnings %d pending %d clearing %d", h.balance(ledger.Earnings, "c_1"),
			h.balance(ledger.PendingPayout, "c_1"), h.balance(ledger.StripeClearing, ""))
	}
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "run-4")
	if len(report.Paid) != 0 || len(h.fake.Transfers()) != 1 {
		t.Fatalf("rerun paid twice: %+v", report)
	}
	payouts, _ := h.svc.Payouts(ctx, store.PayoutQuery{Tenant: tenant, Creator: "c_1"})
	if len(payouts) != 1 || payouts[0].Status != store.PayoutPaid || payouts[0].SettleEntryID == "" {
		t.Fatalf("payouts: %+v", payouts)
	}
}

func payableCreator(t *testing.T, configure ...func(*store.TenantConfig)) *harness {
	h := newHarness(t, append([]func(*store.TenantConfig){func(c *store.TenantConfig) {
		c.HoldDays, c.MinPayout, c.ReviewFirstPayoutAt = 0, 100, 1_000_000
	}}, configure...)...)
	h.buy("u_1", "big", "buy")
	h.clock.Advance(2 * time.Hour)
	if _, err := h.tip("u_1", "c_1", 1000, "tip"); err != nil {
		t.Fatal(err)
	}
	h.verifiedCreator("c_1")
	return h
}

func TestPayoutRefusedUnwinds(t *testing.T) {
	h := payableCreator(t)
	h.fake.Fail("/v1/transfers", http.StatusBadRequest, "account_invalid")
	report, err := h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	if err != nil || len(report.Failed) != 1 {
		t.Fatalf("report: %+v %v", report, err)
	}
	if h.balance(ledger.Earnings, "c_1") != 850 || h.balance(ledger.PendingPayout, "c_1") != 0 {
		t.Fatalf("not unwound: %d %d", h.balance(ledger.Earnings, "c_1"), h.balance(ledger.PendingPayout, "c_1"))
	}
	payouts, _ := h.svc.Payouts(ctx, store.PayoutQuery{Tenant: tenant})
	if payouts[0].Status != store.PayoutFailed || payouts[0].Failure == "" {
		t.Fatalf("payout: %+v", payouts[0])
	}
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "r2")
	if len(report.Paid) != 1 {
		t.Fatalf("next run: %+v", report)
	}
}

func TestPayoutUnknownOutcomeRetriesOnce(t *testing.T) {
	h := payableCreator(t)
	h.fake.Fail("/v1/transfers", http.StatusInternalServerError, "api_error")
	report, _ := h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	if len(report.Pending) != 1 || h.balance(ledger.PendingPayout, "c_1") != 850 {
		t.Fatalf("pending: %+v", report)
	}
	h.clock.Advance(time.Hour)
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "r2")
	if len(report.Paid) != 1 || len(h.fake.Transfers()) != 1 || h.balance(ledger.PendingPayout, "c_1") != 0 {
		t.Fatalf("retry: %+v transfers %d", report, len(h.fake.Transfers()))
	}
}

func TestPayoutStuckPastIdempotencyWindowGoesToAPerson(t *testing.T) {
	h := payableCreator(t)
	h.fake.Fail("/v1/transfers", http.StatusInternalServerError, "api_error")
	h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	h.clock.Advance(service.IdempotencyWindow + time.Minute)
	report, _ := h.svc.RunPayouts(ctx, "scheduler", tenant, "r2")
	if len(report.Paid) != 0 || len(h.fake.Transfers()) != 0 || len(h.openReviews("payee:c_1")) != 1 {
		t.Fatalf("stuck payout retried blind: %+v", report)
	}
}

func TestFirstLargePayoutNeedsReview(t *testing.T) {
	h := payableCreator(t, func(c *store.TenantConfig) { c.ReviewFirstPayoutAt = 500 })
	report, _ := h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	open := h.openReviews("payee:c_1")
	if report.Skipped["under_review"] != 1 || len(open) != 1 || open[0].Reason != "first_payout" {
		t.Fatalf("first payout: %+v %+v", report, open)
	}
	if _, err := h.svc.ResolveReview(ctx, "key:reviewer", tenant, open[0].ID, true, "ok"); err != nil {
		t.Fatal(err)
	}
	report, _ = h.svc.RunPayouts(ctx, "scheduler", tenant, "r2")
	if len(report.Paid) != 1 {
		t.Fatalf("after approval: %+v", report)
	}
}

func TestRejectedReviewFreezesPayee(t *testing.T) {
	h := payableCreator(t, func(c *store.TenantConfig) { c.ReviewTipAt = 900 })
	open := h.openReviews("payee:c_1")
	if len(open) != 1 || open[0].Reason != "large_tip" {
		t.Fatalf("large tip review: %+v", open)
	}
	if _, err := h.svc.ResolveReview(ctx, "key:reviewer", tenant, open[0].ID, false, "mule"); err != nil {
		t.Fatal(err)
	}
	report, _ := h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	if report.Skipped["frozen"] != 1 || len(h.fake.Transfers()) != 0 {
		t.Fatalf("frozen payee paid: %+v", report)
	}
	h.grant("u_2", 1000, "g")
	if _, err := h.tip("u_2", "c_1", 100, "t2"); !errors.Is(err, service.ErrFrozen) {
		t.Fatalf("tip to frozen creator: %v", err)
	}
}

func TestTransferReversed(t *testing.T) {
	h := payableCreator(t)
	h.svc.RunPayouts(ctx, "scheduler", tenant, "r1")
	transfers := h.fake.Transfers()
	for id := range transfers {
		h.deliver(h.fake.ReverseTransfer(id))
		h.deliver(h.fake.ReverseTransfer(id))
	}
	payouts, _ := h.svc.Payouts(ctx, store.PayoutQuery{Tenant: tenant})
	if h.balance(ledger.Earnings, "c_1") != 850 || payouts[0].Status != store.PayoutReturned || len(h.openReviews("payee:c_1")) != 1 {
		t.Fatalf("reversal: earnings %d %+v", h.balance(ledger.Earnings, "c_1"), payouts[0])
	}
}

func TestFreshWalletDrainIsFlagged(t *testing.T) {
	h := newHarness(t)
	h.buy("u_1", "big", "buy")
	if _, err := h.tip("u_1", "c_1", 1000, "tip"); err != nil {
		t.Fatal(err)
	}
	open := h.openReviews("payee:c_1")
	if len(open) != 1 || open[0].Reason != "fresh_wallet_drain" {
		t.Fatalf("drain: %+v", open)
	}
}

func TestKeys(t *testing.T) {
	h := newHarness(t)
	key, plaintext, err := h.svc.IssueKey(ctx, "operator", tenant, "backend", []store.Scope{store.ScopeTipsWrite, store.ScopeTipsWrite})
	if err != nil || len(key.Scopes) != 1 {
		t.Fatalf("issue: %+v %v", key, err)
	}
	stored, _ := h.store.GetKey(ctx, key.ID)
	if stored.Hash == "" || stored.Hash == plaintext {
		t.Fatal("key stored in the clear")
	}
	resolved, err := h.svc.ResolveKey(ctx, plaintext)
	if err != nil || resolved.ID != key.ID || !resolved.Allows(store.ScopeTipsWrite) || resolved.Allows(store.ScopePayoutsWrite) {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	tampered := plaintext[:len(plaintext)-1] + "A"
	if plaintext[len(plaintext)-1] == 'A' {
		tampered = plaintext[:len(plaintext)-1] + "B"
	}
	for _, bad := range []string{"", "mqk_" + plaintext[4:], tampered, "mtk_0000000000000000_" + plaintext[len(plaintext)-43:]} {
		if _, err := h.svc.ResolveKey(ctx, bad); !errors.Is(err, service.ErrInvalidKey) {
			t.Errorf("%q resolved: %v", bad, err)
		}
	}
	if err := h.svc.RevokeKey(ctx, "operator", tenant, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ResolveKey(ctx, plaintext); !errors.Is(err, service.ErrInvalidKey) {
		t.Fatal("revoked key resolved")
	}
	if _, _, err := h.svc.IssueKey(ctx, "operator", tenant, "x", []store.Scope{"everything"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	h := newHarness(t)
	base := service.DefaultConfig()
	cases := map[string]func(*store.TenantConfig){
		"fees take everything": func(c *store.TenantConfig) { c.TenantFee = 9500 },
		"pack currency":        func(c *store.TenantConfig) { c.Packs = []store.Pack{{ID: "p", Currency: "eur", Price: 100}} },
		"bonus over price": func(c *store.TenantConfig) {
			c.Packs = []store.Pack{{ID: "p", Currency: "usd", Price: 100, Bonus: 101}}
		},
		"duplicate pack": func(c *store.TenantConfig) {
			c.Packs = []store.Pack{{ID: "p", Currency: "usd", Price: 1}, {ID: "p", Currency: "usd", Price: 2}}
		},
		"hold too long": func(c *store.TenantConfig) { c.HoldDays = 91 },
		"liability":     func(c *store.TenantConfig) { c.ChargebackLiability = "nobody" },
		"tip bounds":    func(c *store.TenantConfig) { c.TipMax = c.TipMin - 1 },
	}
	for name, mutate := range cases {
		c := base
		mutate(&c)
		if _, err := h.svc.UpdateConfig(ctx, "key:test", tenant, c); !errors.Is(err, service.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExportAndErase(t *testing.T) {
	h := newHarness(t)
	h.buy("u_1", "big", "buy")
	h.tip("u_1", "c_1", 300, "tip")
	h.verifiedCreator("u_1")
	export, err := h.svc.ExportUser(ctx, tenant, "u_1")
	if err != nil {
		t.Fatal(err)
	}
	if export.Wallet.Balances["usd"] != 800 || len(export.Purchases) != 1 || len(export.TipsSent) != 1 ||
		export.TipsSent[0].Message != "nice" || len(export.Entries) != 2 || export.Payee == nil {
		t.Fatalf("export: %+v", export)
	}
	erased, err := h.svc.EraseUser(ctx, "key:dsar", tenant, "u_1")
	if err != nil || erased.TipMessagesErased != 1 || len(erased.Retained) == 0 {
		t.Fatalf("erase: %+v %v", erased, err)
	}
	export, _ = h.svc.ExportUser(ctx, tenant, "u_1")
	if export.TipsSent[0].Message != "" || export.TipsSent[0].Amount != 300 || len(export.Entries) != 2 {
		t.Fatalf("after erasure: %+v", export.TipsSent)
	}
}

// Property: whatever sequence of purchases, tips, spends, grants, refunds,
// disputes and payouts happens, the books balance and nobody is overdrawn.
func TestRandomOperationsKeepTheBooks(t *testing.T) {
	seen := map[ledger.EntryKind]int{}
	defer func() {
		for _, kind := range []ledger.EntryKind{ledger.KindPurchase, ledger.KindTip, ledger.KindSpend, ledger.KindRefund,
			ledger.KindChargeback, ledger.KindPayoutInitiated, ledger.KindPayoutTransferred} {
			if seen[kind] == 0 {
				t.Errorf("the random walk never posted a %s entry", kind)
			}
		}
	}()
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h := newHarness(t, func(c *store.TenantConfig) {
				c.SenderTipsPerMin, c.RoomTipsPerMin, c.SenderDailyCap, c.RoomDailyCap = 1000, 1000, 10_000_000, 10_000_000
				c.PurchasesPerHour, c.HoldDays, c.MinPayout, c.ReviewFirstPayoutAt, c.RepeatPairPerHour = 1000, 1, 100, 10_000_000, 1000
				c.ReviewTipAt = 10_000_000
			})
			random := rand.New(rand.NewSource(seed))
			users := []string{"u_1", "u_2", "u_3"}
			creators := []string{"c_1", "c_2"}
			for _, c := range creators {
				h.verifiedCreator(c)
			}
			var purchases []store.Purchase
			var intents []string
			for step := 0; step < 120; step++ {
				user := users[random.Intn(len(users))]
				key := fmt.Sprintf("op-%d", step)
				switch random.Intn(8) {
				case 0, 1:
					if state, _ := h.svc.Wallet(ctx, tenant, user); state.Wallet.Frozen {
						if _, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: user, Pack: "small",
							Mode: service.ModePaymentIntent, IdempotencyKey: key}); !errors.Is(err, service.ErrFrozen) {
							t.Fatalf("frozen wallet bought: %v", err)
						}
						continue
					}
					p, intent := h.buy(user, []string{"small", "big"}[random.Intn(2)], key)
					purchases, intents = append(purchases, p), append(intents, intent)
				case 2, 3:
					h.tip(user, creators[random.Intn(2)], ledger.Amount(50+random.Intn(800)), key)
				case 4:
					h.svc.Spend(ctx, "key:test", tenant, service.SpendRequest{User: user, Amount: ledger.Amount(1 + random.Intn(400)), IdempotencyKey: key})
				case 5:
					if len(purchases) > 0 {
						i := random.Intn(len(purchases))
						h.svc.Refund(ctx, "key:test", tenant, purchases[i].ID, ledger.Amount(random.Intn(300)), key)
					}
				case 6:
					if len(intents) > 0 && random.Intn(3) == 0 {
						i := random.Intn(len(intents))
						d, _ := h.fake.Dispute(intents[i], int64(purchases[i].Price))
						h.deliver(d)
					}
				case 7:
					h.clock.Advance(time.Duration(random.Intn(48)) * time.Hour)
					if _, err := h.svc.RunPayouts(ctx, "scheduler", tenant, key); err != nil {
						t.Fatal(err)
					}
				}
				h.checkBooks()
			}
			entries, _ := h.store.ListEntries(ctx, store.EntryFilter{Tenant: tenant})
			for _, e := range entries {
				seen[e.Kind]++
			}
		})
	}
}
