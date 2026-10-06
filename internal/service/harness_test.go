// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moqom-cloud/motom/events"
	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/memory"
	"github.com/moqom-cloud/motom/internal/stripe/stripefake"
	"github.com/moqom-cloud/motom/ledger"
)

const tenant = "t_1"

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type harness struct {
	t     *testing.T
	svc   *service.Service
	store *memory.Store
	fake  *stripefake.Fake
	clock *clock

	mu     sync.Mutex
	events []events.Tip
}

// newHarness is a tenant selling two packs (500 for 500 credits, 1000 for
// 1100), taking a 5% platform fee and a 10% tenant fee on tips, against the
// fake Stripe.
func newHarness(t *testing.T, configure ...func(*store.TenantConfig)) *harness {
	t.Helper()
	h := &harness{t: t, store: memory.New(), clock: &clock{at: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}}
	h.fake = stripefake.New(h.clock.Now)
	t.Cleanup(h.fake.Server.Close)
	svc, err := service.New(service.Config{
		Store: h.store, Stripe: h.fake.Client(), WebhookSecrets: []string{h.fake.WebhookSecret, h.fake.ConnectSecret},
		Broadcaster: events.BroadcasterFunc(func(_ context.Context, tip events.Tip) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, tip)
			return nil
		}),
		MinHoldDays: 0, Now: h.clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	if _, err := svc.CreateTenant(ctx, "operator", tenant, []ledger.Currency{"usd"}, 500); err != nil {
		t.Fatal(err)
	}
	config := service.DefaultConfig()
	config.TenantFee = 1000
	config.Packs = []store.Pack{
		{ID: "small", Name: "500 credits", Currency: "usd", Price: 500},
		{ID: "big", Name: "1100 credits", Currency: "usd", Price: 1000, Bonus: 100},
	}
	for _, f := range configure {
		f(&config)
	}
	if _, err := svc.UpdateConfig(ctx, "key:test", tenant, config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.checkBooks)
	return h
}

func (h *harness) deliver(d stripefake.Delivery) {
	h.t.Helper()
	if err := h.svc.HandleWebhook(ctx, d.Payload, d.Header); err != nil {
		h.t.Fatalf("webhook: %v", err)
	}
}

// buy purchases a pack through Checkout and pays it. It returns the
// purchase and the payment intent Stripe made.
func (h *harness) buy(user, pack, key string) (store.Purchase, string) {
	h.t.Helper()
	result, err := h.svc.Purchase(ctx, "key:test", tenant, service.PurchaseRequest{User: user, Pack: pack, Mode: service.ModeCheckout,
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/cancel", IdempotencyKey: key})
	if err != nil {
		h.t.Fatal(err)
	}
	if !strings.HasPrefix(result.CheckoutURL, "https://checkout.stripe.test/") {
		h.t.Fatalf("checkout url %q", result.CheckoutURL)
	}
	delivery, intent := h.fake.CompleteCheckout(result.Purchase.StripeCheckout)
	h.deliver(delivery)
	p, err := h.svc.GetPurchase(ctx, tenant, result.Purchase.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return p, intent
}

func (h *harness) grant(user string, amount ledger.Amount, key string) {
	h.t.Helper()
	if _, err := h.svc.Grant(ctx, "key:test", tenant, service.GrantRequest{User: user, Amount: amount, IdempotencyKey: key}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) tip(sender, creator string, amount ledger.Amount, key string) (service.TipResult, error) {
	return h.svc.Tip(ctx, "key:test", tenant, service.TipRequest{Room: "room_1", Sender: sender, Creator: creator,
		Amount: amount, Message: "nice", IdempotencyKey: key})
}

// balance is an account's natural balance; zero if it was never opened.
func (h *harness) balance(kind ledger.AccountKind, owner string) ledger.Amount {
	h.t.Helper()
	id := ledger.AccountFor(tenant, kind, owner, "usd")
	account, err := h.store.GetAccount(ctx, id)
	if err != nil {
		return 0
	}
	b, err := h.store.Balance(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return account.Normal(b.Amount)
}

func (h *harness) wallet(user string) ledger.Amount { return h.balance(ledger.Wallet, user) }

// checkBooks is the invariant every test ends on: the trial balance is zero
// and no wallet, earnings or pending payout account is negative.
func (h *harness) checkBooks() {
	h.t.Helper()
	trial, err := h.svc.TrialBalance(ctx, tenant)
	if err != nil {
		h.t.Fatal(err)
	}
	for currency, sum := range trial {
		if sum != 0 {
			h.t.Errorf("trial balance %s = %d", currency, sum)
		}
	}
	accounts, _ := h.store.ListAccounts(ctx, tenant, "")
	for _, a := range accounts {
		b, _ := h.store.Balance(ctx, a.ID)
		if !a.Kind.MayOverdraw() && a.Normal(b.Amount) < 0 {
			h.t.Errorf("%s is overdrawn: %d", a.ID, a.Normal(b.Amount))
		}
	}
}

func (h *harness) openReviews(subject string) []store.Review {
	h.t.Helper()
	reviews, err := h.store.ListReviews(ctx, tenant, store.ReviewOpen, subject)
	if err != nil {
		h.t.Fatal(err)
	}
	return reviews
}

func (h *harness) broadcasts() []events.Tip {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.Tip(nil), h.events...)
}

// verifiedCreator onboards a creator and has Stripe report them verified.
func (h *harness) verifiedCreator(creator string) store.Payee {
	h.t.Helper()
	link, payee, err := h.svc.Onboard(ctx, "key:test", tenant, creator, "US", "https://app.example/refresh", "https://app.example/done")
	if err != nil || !strings.Contains(link, payee.StripeAccount) || payee.KYC != store.KYCPending {
		h.t.Fatalf("onboard: %q %+v %v", link, payee, err)
	}
	h.deliver(h.fake.UpdateAccount(payee.StripeAccount, true, true, ""))
	payee, err = h.store.GetPayee(ctx, tenant, creator)
	if err != nil || payee.KYC != store.KYCVerified || !payee.Payable() {
		h.t.Fatalf("verified: %+v %v", payee, err)
	}
	return payee
}
