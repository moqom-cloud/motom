// Copyright 2026 Elliott Wave LLC. All rights reserved.

package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moqom-cloud/motom/client"
	"github.com/moqom-cloud/motom/events"
	"github.com/moqom-cloud/motom/internal/httpapi"
	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/memory"
	"github.com/moqom-cloud/motom/internal/stripe/stripefake"
	"github.com/moqom-cloud/motom/ledger"
)

const tenant = "t_e2e"

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

type world struct {
	t      *testing.T
	svc    *service.Service
	fake   *stripefake.Fake
	clock  *clock
	server *httptest.Server
	admin  *client.Client

	mu     sync.Mutex
	events []events.Tip
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, clock: &clock{at: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}}
	w.fake = stripefake.New(w.clock.Now)
	t.Cleanup(w.fake.Server.Close)
	svc, err := service.New(service.Config{
		Store: memory.New(), Stripe: w.fake.Client(), WebhookSecrets: []string{w.fake.WebhookSecret, w.fake.ConnectSecret},
		Broadcaster: events.BroadcasterFunc(func(_ context.Context, tip events.Tip) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.events = append(w.events, tip)
			return nil
		}),
		Now: w.clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	w.server = httptest.NewServer(httpapi.Handler(svc, httpapi.Options{Logger: quiet, Now: w.clock.Now}))
	t.Cleanup(w.server.Close)
	if _, err := svc.CreateTenant(ctx, "operator", tenant, []ledger.Currency{"usd"}, 500); err != nil {
		t.Fatal(err)
	}
	_, plaintext, err := svc.IssueKey(ctx, "operator", tenant, "bootstrap", []store.Scope{store.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	w.admin = client.New(w.server.URL, plaintext)
	return w
}

func (w *world) key(name string, scopes ...string) *client.Client {
	w.t.Helper()
	_, plaintext, err := w.admin.IssueKey(ctx, name, scopes...)
	if err != nil {
		w.t.Fatal(err)
	}
	return client.New(w.server.URL, plaintext)
}

func (w *world) post(d stripefake.Delivery) int {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, w.server.URL+"/v1/motom/webhooks/stripe", bytes.NewReader(d.Payload))
	req.Header.Set("Stripe-Signature", d.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (w *world) deliver(d stripefake.Delivery) {
	w.t.Helper()
	if status := w.post(d); status != http.StatusOK {
		w.t.Fatalf("webhook status %d", status)
	}
}

// buy purchases a pack through Checkout and has Stripe confirm it. It
// returns the purchase and Stripe's payment intent.
func (w *world) buy(c *client.Client, user, pack string) (client.Purchase, string) {
	w.t.Helper()
	p, err := c.Purchase(ctx, client.PurchaseRequest{User: user, Pack: pack, Mode: client.ModeCheckout,
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/no"}, client.NewIdempotencyKey())
	if err != nil || p.CheckoutURL == "" || p.Status != "pending" {
		w.t.Fatalf("purchase %+v %v", p, err)
	}
	stored, err := w.svc.GetPurchase(ctx, tenant, p.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	delivery, intent := w.fake.CompleteCheckout(stored.StripeCheckout)
	w.deliver(delivery)
	p, err = c.GetPurchase(ctx, p.ID)
	if err != nil || p.Status != "succeeded" {
		w.t.Fatalf("paid purchase %+v %v", p, err)
	}
	return p, intent
}

// strict GETs path and decodes it refusing fields the client type lacks, so
// the client's types cannot drift from what the server sends.
func (w *world) strict(c *client.Client, path string, into any) {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, w.server.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		w.t.Fatalf("GET %s: %v", path, err)
	}
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	if !client.IsCode(err, want) {
		t.Fatalf("got %v, want code %s", err, want)
	}
}

func TestEndToEnd(t *testing.T) {
	w := newWorld(t)

	tenantState, err := w.admin.Tenant(ctx)
	if err != nil || tenantState.ID != tenant || tenantState.PlatformFee != 500 {
		t.Fatalf("tenant %+v %v", tenantState, err)
	}
	config := tenantState.Config
	config.TenantFee = 1000
	config.MinPayout = 500
	config.Packs = []client.Pack{
		{ID: "small", Name: "500 credits", Currency: "usd", Price: 500},
		{ID: "big", Name: "1100 credits", Currency: "usd", Price: 1000, Bonus: 100},
	}
	if _, err := w.admin.UpdateConfig(ctx, config); err != nil {
		t.Fatal(err)
	}

	app := w.key("app", "wallets:read", "wallets:write", "purchases:write", "refunds:write", "tips:read", "tips:write",
		"payouts:read", "payouts:write")
	moderator := w.key("moderator", "reviews:read", "reviews:write", "wallets:read")
	privacy := w.key("privacy", "dsar")

	_, err = app.Tenant(ctx)
	code(t, err, client.CodeForbidden)
	_, err = client.New(w.server.URL, "mtk_0000000000000000_"+strings.Repeat("a", 43)).Wallet(ctx, "u_viewer")
	code(t, err, client.CodeUnauthorized)

	packs, err := app.Packs(ctx)
	if err != nil || len(packs) != 2 {
		t.Fatalf("packs %v %v", packs, err)
	}

	// Buy credits; Stripe's retry of the same event changes nothing.
	purchase, _ := w.buy(app, "u_viewer", "big")
	if purchase.Credits != 1100 {
		t.Fatalf("credits %d", purchase.Credits)
	}
	wallet, err := app.Wallet(ctx, "u_viewer")
	if err != nil || wallet.Balances["usd"] != 1100 {
		t.Fatalf("wallet %+v %v", wallet, err)
	}

	// A forged webhook is refused.
	forged := stripefake.Delivery{Payload: []byte(`{"id":"evt_forged","type":"payment_intent.succeeded"}`),
		Header: "t=1,v1=00"}
	if status := w.post(forged); status != http.StatusBadRequest {
		t.Fatalf("forged webhook status %d", status)
	}

	// Tip: 1000 splits 50 platform, 100 tenant, 850 creator, and replays.
	// The wallet is aged first: draining a brand-new wallet is flagged.
	w.clock.Advance(2 * time.Hour)
	_, err = app.Tip(ctx, client.TipRequest{Room: "room_1", Sender: "u_viewer", Creator: "u_creator", Amount: 1000}, "")
	code(t, err, client.CodeInvalid)
	tip, err := app.Tip(ctx, client.TipRequest{Room: "room_1", Sender: "u_viewer", Creator: "u_creator", Amount: 1000,
		Currency: "usd", Message: "great set"}, "tip-1")
	if err != nil || tip.Replayed || tip.Event.Type != events.TipType || tip.Event.Amount != 1000 {
		t.Fatalf("tip %+v %v", tip, err)
	}
	again, err := app.Tip(ctx, client.TipRequest{Room: "room_1", Sender: "u_viewer", Creator: "u_creator", Amount: 1000,
		Currency: "usd", Message: "great set"}, "tip-1")
	if err != nil || !again.Replayed || again.Tip.ID != tip.Tip.ID {
		t.Fatalf("replay %+v %v", again, err)
	}
	_, err = app.Tip(ctx, client.TipRequest{Room: "room_1", Sender: "u_viewer", Creator: "u_creator", Amount: 999,
		Currency: "usd"}, "tip-1")
	code(t, err, client.CodeIdempotencyMismatch)
	_, err = app.Tip(ctx, client.TipRequest{Room: "room_1", Sender: "u_viewer", Creator: "u_creator", Amount: 500,
		Currency: "usd"}, "tip-2")
	code(t, err, client.CodeInsufficientFunds)
	if w.mu.Lock(); len(w.events) != 1 {
		t.Fatalf("broadcast %d events", len(w.events))
	}
	w.mu.Unlock()
	tips, err := app.RoomTips(ctx, "room_1", 10)
	if err != nil || len(tips) != 1 || tips[0].Message != "great set" {
		t.Fatalf("room tips %+v %v", tips, err)
	}

	// Onboard the creator, wait out the hold, pay out.
	link, err := app.Onboard(ctx, "u_creator", "US", "https://app.example/refresh", "https://app.example/done")
	if err != nil || link == "" {
		t.Fatalf("onboard %q %v", link, err)
	}
	creator, err := app.Creator(ctx, "u_creator")
	if err != nil || creator.KYC != "pending" || creator.Payable {
		t.Fatalf("creator %+v %v", creator, err)
	}
	w.deliver(w.fake.UpdateAccount(creator.StripeAccount, true, true, ""))
	creator, _ = app.Creator(ctx, "u_creator")
	if !creator.Payable || len(creator.Earnings) != 1 || creator.Earnings[0].Balance != 850 || creator.Earnings[0].Held != 850 {
		t.Fatalf("verified creator %+v", creator)
	}
	report, err := app.RunPayouts(ctx, "run-1")
	if err != nil || len(report.Paid) != 0 {
		t.Fatalf("held run %+v %v", report, err)
	}
	w.clock.Advance(8 * 24 * time.Hour)
	report, err = app.RunPayouts(ctx, "run-2")
	if err != nil || len(report.Paid) != 1 {
		t.Fatalf("run %+v %v", report, err)
	}
	payouts, err := app.Payouts(ctx, "u_creator", "paid", 0)
	if err != nil || len(payouts) != 1 || payouts[0].Amount != 850 || payouts[0].StripeTransfer == "" {
		t.Fatalf("payouts %+v %v", payouts, err)
	}
	if transfers := w.fake.Transfers(); len(transfers) != 1 {
		t.Fatalf("stripe transfers %d", len(transfers))
	}
	report, _ = app.RunPayouts(ctx, "run-2")
	if len(report.Paid) != 0 || len(w.fake.Transfers()) != 1 {
		t.Fatalf("rerun paid again: %+v", report)
	}

	// Refund a purchase in full.
	small, _ := w.buy(app, "u_refund", "small")
	refunded, entry, err := app.Refund(ctx, small.ID, 0, "refund-1")
	if err != nil || refunded.Status != "refunded" || refunded.Refunded != 500 || entry.Kind != "refund" {
		t.Fatalf("refund %+v %+v %v", refunded, entry, err)
	}
	if wallet, _ := app.Wallet(ctx, "u_refund"); wallet.Balances["usd"] != 0 {
		t.Fatalf("refunded wallet %+v", wallet)
	}

	// A chargeback freezes the wallet and opens a review; approving it
	// unfreezes.
	_, intent := w.buy(app, "u_disputer", "small")
	dispute, _ := w.fake.Dispute(intent, 500)
	w.deliver(dispute)
	if wallet, _ := app.Wallet(ctx, "u_disputer"); !wallet.Frozen || wallet.Balances["usd"] != 0 {
		t.Fatalf("disputed wallet %+v", wallet)
	}
	_, err = app.Spend(ctx, "u_disputer", client.SpendRequest{Amount: 1, Currency: "usd"}, "spend-1")
	code(t, err, client.CodeFrozen)
	reviews, err := moderator.Reviews(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var chargeback *client.Review
	for i := range reviews {
		if reviews[i].Reason == "chargeback" {
			chargeback = &reviews[i]
		}
	}
	if chargeback == nil {
		t.Fatalf("no chargeback review in %+v", reviews)
	}
	if _, err := moderator.ResolveReview(ctx, chargeback.ID, true, "customer confirmed"); err != nil {
		t.Fatal(err)
	}
	if wallet, _ := moderator.Wallet(ctx, "u_disputer"); wallet.Frozen {
		t.Fatalf("still frozen %+v", wallet)
	}

	// Spend and grant.
	if _, err := app.Grant(ctx, "u_viewer", client.GrantRequest{Amount: 200, Currency: "usd", Memo: "welcome"}, "grant-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Spend(ctx, "u_viewer", client.SpendRequest{Amount: 250, Currency: "usd", Reference: "sticker"}, "spend-2"); err != nil {
		t.Fatal(err)
	}
	if wallet, _ := app.Wallet(ctx, "u_viewer"); wallet.Balances["usd"] != 50 {
		t.Fatalf("after spend %+v", wallet)
	}
	entries, err := app.WalletEntries(ctx, "u_viewer", 0)
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries %d %v", len(entries), err)
	}

	// Data subject requests.
	export, err := privacy.ExportUser(ctx, "u_creator")
	if err != nil || export.Creator == nil || len(export.TipsReceived) != 1 || len(export.Payouts) != 1 {
		t.Fatalf("export %+v %v", export, err)
	}
	erasure, err := privacy.EraseUser(ctx, "u_viewer")
	if err != nil || erasure.TipMessagesErased != 1 || len(erasure.Retained) == 0 {
		t.Fatalf("erase %+v %v", erasure, err)
	}
	if tips, _ := app.RoomTips(ctx, "room_1", 10); tips[0].Message != "" {
		t.Fatalf("message survived erasure: %q", tips[0].Message)
	}

	// The client's types match the server's JSON exactly.
	w.strict(app, "/v1/motom/wallets/u_viewer", &client.Wallet{})
	w.strict(app, "/v1/motom/wallets/u_viewer/entries", &struct {
		Entries []client.Entry `json:"entries"`
	}{})
	w.strict(app, "/v1/motom/purchases/"+purchase.ID, &client.Purchase{})
	w.strict(app, "/v1/motom/creators/u_creator", &client.Creator{})
	w.strict(app, "/v1/motom/payouts", &struct {
		Payouts []client.Payout `json:"payouts"`
	}{})
	w.strict(app, "/v1/motom/rooms/room_1/tips", &struct {
		Tips []client.Tip `json:"tips"`
	}{})
	w.strict(w.admin, "/v1/motom/tenant", &client.Tenant{})
	w.strict(w.admin, "/v1/motom/keys", &struct {
		Keys []client.Key `json:"keys"`
	}{})
	w.strict(w.admin, "/v1/motom/audit", &struct {
		Audit []client.AuditRecord `json:"audit"`
	}{})
	w.strict(moderator, "/v1/motom/reviews?status=all", &struct {
		Reviews []client.Review `json:"reviews"`
	}{})
	w.strict(privacy, "/v1/motom/users/u_creator/export", &client.UserExport{})

	// The books balance and every movement is audited.
	trial, err := w.admin.TrialBalance(ctx)
	if err != nil || !trial.Balanced || trial.Sums["usd"] != 0 {
		t.Fatalf("trial balance %+v %v", trial, err)
	}
	audit, err := w.admin.Audit(ctx, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, a := range audit {
		actions[a.Action] = true
	}
	for _, want := range []string{"ledger.purchase", "ledger.tip", "ledger.refund", "ledger.chargeback", "ledger.payout_initiated", "ledger.spend", "ledger.grant"} {
		if !actions[want] {
			t.Errorf("no %s in audit log", want)
		}
	}

	// A revoked key stops working at once.
	keys, _ := w.admin.Keys(ctx)
	for _, k := range keys {
		if k.Name == "privacy" {
			if err := w.admin.RevokeKey(ctx, k.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err = privacy.ExportUser(ctx, "u_creator")
	code(t, err, client.CodeUnauthorized)
}
