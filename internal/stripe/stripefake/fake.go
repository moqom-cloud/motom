// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package stripefake is a Stripe stand-in for end-to-end tests: the
// endpoints MOTOM calls, Stripe's idempotency-key replay, one-shot failure
// injection, and signed webhook deliveries for the events MOTOM handles.
package stripefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/motom/internal/stripe"
)

// SecretKey is the only key the fake accepts. It is not a Stripe key.
const SecretKey = "sk_test_motom_fake"

// Fake is a running fake.
type Fake struct {
	Server *httptest.Server
	// WebhookSecret and ConnectSecret sign deliveries from the platform and
	// Connect endpoints.
	WebhookSecret string
	ConnectSecret string
	Now           func() time.Time

	mu        sync.Mutex
	next      int
	replay    map[string]recorded
	failures  map[string][]failure
	calls     map[string]int
	sessions  map[string]session
	intents   map[string]stripe.PaymentIntent
	refunds   map[string]stripe.Refund
	accounts  map[string]stripe.Account
	transfers map[string]stripe.Transfer
	events    int
}

type recorded struct {
	status int
	body   []byte
}

type failure struct {
	status int
	code   string
}

type session struct {
	amount   int64
	currency string
	metadata map[string]string
}

// New starts a fake. Close it with Server.Close.
func New(now func() time.Time) *Fake {
	f := &Fake{
		WebhookSecret: "whsec_motom_fake_platform", ConnectSecret: "whsec_motom_fake_connect", Now: now,
		replay: map[string]recorded{}, failures: map[string][]failure{}, calls: map[string]int{},
		sessions: map[string]session{}, intents: map[string]stripe.PaymentIntent{}, refunds: map[string]stripe.Refund{},
		accounts: map[string]stripe.Account{}, transfers: map[string]stripe.Transfer{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Client is a stripe.Client pointed at the fake.
func (f *Fake) Client() *stripe.Client {
	return &stripe.Client{SecretKey: SecretKey, BaseURL: f.Server.URL, HTTP: f.Server.Client()}
}

// Fail makes the next call to path answer status with code.
func (f *Fake) Fail(path string, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[path] = append(f.failures[path], failure{status, code})
}

// Calls counts requests that reached path, replays included.
func (f *Fake) Calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

// Transfers returns every transfer made, by ID.
func (f *Fake) Transfers() map[string]stripe.Transfer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]stripe.Transfer{}
	for k, v := range f.transfers {
		out[k] = v
	}
	return out
}

// Refunds returns every refund made, by ID.
func (f *Fake) Refunds() map[string]stripe.Refund {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]stripe.Refund{}
	for k, v := range f.refunds {
		out[k] = v
	}
	return out
}

func (f *Fake) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s_fake%d", prefix, f.next)
}

func metadata(form map[string][]string, prefix string) map[string]string {
	out := map[string]string{}
	for key, values := range form {
		if rest, ok := strings.CutPrefix(key, prefix+"["); ok && strings.HasSuffix(rest, "]") && len(values) > 0 {
			out[strings.TrimSuffix(rest, "]")] = values[0]
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) []byte {
	encoded, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
	return encoded
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	user, _, ok := r.BasicAuth()
	if !ok || user != SecretKey {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": "invalid api key"}})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": err.Error()}})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.URL.Path]++
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if previous, ok := f.replay[r.URL.Path+"|"+key]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(previous.status)
			_, _ = w.Write(previous.body)
			return
		}
	}
	if queued := f.failures[r.URL.Path]; len(queued) > 0 {
		f.failures[r.URL.Path] = queued[1:]
		// Like Stripe, a 4xx is remembered against the key and a 5xx is not.
		body := writeJSON(w, queued[0].status, map[string]any{"error": map[string]string{"type": "api_error", "code": queued[0].code, "message": "injected"}})
		if key != "" && queued[0].status < 500 {
			f.replay[r.URL.Path+"|"+key] = recorded{queued[0].status, body}
		}
		return
	}
	status, response := f.route(r)
	body := writeJSON(w, status, response)
	if key != "" {
		f.replay[r.URL.Path+"|"+key] = recorded{status, body}
	}
}

func (f *Fake) route(r *http.Request) (int, any) {
	amount, _ := strconv.ParseInt(r.Form.Get("amount"), 10, 64)
	switch r.URL.Path {
	case "/v1/checkout/sessions":
		unit, _ := strconv.ParseInt(r.Form.Get("line_items[0][price_data][unit_amount]"), 10, 64)
		id := f.id("cs")
		f.sessions[id] = session{unit, r.Form.Get("line_items[0][price_data][currency]"), metadata(r.Form, "payment_intent_data[metadata]")}
		return http.StatusOK, stripe.Checkout{ID: id, URL: "https://checkout.stripe.test/" + id}
	case "/v1/payment_intents":
		intent := stripe.PaymentIntent{ID: f.id("pi"), Status: "requires_payment_method", Amount: amount,
			Currency: r.Form.Get("currency"), Metadata: metadata(r.Form, "metadata")}
		intent.ClientSecret = intent.ID + "_secret_fake"
		f.intents[intent.ID] = intent
		return http.StatusOK, intent
	case "/v1/refunds":
		intent, ok := f.intents[r.Form.Get("payment_intent")]
		if !ok {
			return http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": "resource_missing"}}
		}
		refund := stripe.Refund{ID: f.id("re"), Amount: amount, Status: "succeeded", PaymentIntent: intent.ID}
		f.refunds[refund.ID] = refund
		return http.StatusOK, refund
	case "/v1/accounts":
		account := stripe.Account{ID: f.id("acct"), Metadata: metadata(r.Form, "metadata")}
		f.accounts[account.ID] = account
		return http.StatusOK, account
	case "/v1/account_links":
		if _, ok := f.accounts[r.Form.Get("account")]; !ok {
			return http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": "resource_missing"}}
		}
		return http.StatusOK, map[string]string{"url": "https://connect.stripe.test/onboard/" + r.Form.Get("account")}
	case "/v1/transfers":
		if _, ok := f.accounts[r.Form.Get("destination")]; !ok {
			return http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": "resource_missing"}}
		}
		transfer := stripe.Transfer{ID: f.id("tr"), Amount: amount, Currency: r.Form.Get("currency"),
			Destination: r.Form.Get("destination"), Metadata: metadata(r.Form, "metadata")}
		f.transfers[transfer.ID] = transfer
		return http.StatusOK, transfer
	}
	return http.StatusNotFound, map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": "unknown path " + r.URL.Path}}
}

// Delivery is a signed webhook body and its Stripe-Signature header.
type Delivery struct {
	Payload []byte
	Header  string
	EventID string
}

func (f *Fake) deliver(kind, account string, object any) Delivery {
	f.events++
	secret := f.WebhookSecret
	if account != "" {
		secret = f.ConnectSecret
	}
	raw, _ := json.Marshal(object)
	id := fmt.Sprintf("evt_fake%d", f.events)
	payload, _ := json.Marshal(map[string]any{
		"id": id, "type": kind, "created": f.Now().Unix(), "account": account,
		"data": map[string]json.RawMessage{"object": raw},
	})
	return Delivery{Payload: payload, Header: stripe.SignWebhook(payload, secret, f.Now()), EventID: id}
}

// CompleteCheckout pays a Checkout Session: it creates the payment intent
// Stripe would and returns payment_intent.succeeded.
func (f *Fake) CompleteCheckout(sessionID string) (Delivery, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[sessionID]
	intent := stripe.PaymentIntent{ID: f.id("pi"), Status: "succeeded", Amount: s.amount, AmountReceived: s.amount,
		Currency: s.currency, Metadata: s.metadata}
	intent.LatestCharge = f.id("ch")
	f.intents[intent.ID] = intent
	return f.deliver("payment_intent.succeeded", "", intent), intent.ID
}

// SucceedIntent confirms a payment intent, optionally for a different
// amount than it was created with, and returns payment_intent.succeeded.
func (f *Fake) SucceedIntent(id string, received int64) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	intent := f.intents[id]
	intent.Status, intent.AmountReceived = "succeeded", received
	intent.LatestCharge = f.id("ch")
	f.intents[id] = intent
	return f.deliver("payment_intent.succeeded", "", intent)
}

// FailIntent returns payment_intent.payment_failed.
func (f *Fake) FailIntent(id string) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	intent := f.intents[id]
	intent.Status = "requires_payment_method"
	return f.deliver("payment_intent.payment_failed", "", intent)
}

// RefundOutside is a refund made in the Stripe dashboard: charge.refunded
// with a cumulative amount_refunded MOTOM did not ask for.
func (f *Fake) RefundOutside(intentID string, totalRefunded int64) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	intent := f.intents[intentID]
	return f.deliver("charge.refunded", "", stripe.Charge{ID: intent.LatestCharge, PaymentIntent: intent.ID,
		Amount: intent.AmountReceived, AmountRefunded: totalRefunded, Currency: intent.Currency})
}

// Dispute opens a chargeback and returns charge.dispute.created.
func (f *Fake) Dispute(intentID string, amount int64) (Delivery, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	intent := f.intents[intentID]
	dispute := stripe.Dispute{ID: f.id("dp"), Charge: intent.LatestCharge, PaymentIntent: intent.ID,
		Amount: amount, Currency: intent.Currency, Status: "needs_response", Reason: "fraudulent"}
	return f.deliver("charge.dispute.created", "", dispute), dispute.ID
}

// CloseDispute returns charge.dispute.closed with status won or lost.
func (f *Fake) CloseDispute(intentID, disputeID string, amount int64, status string) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	intent := f.intents[intentID]
	return f.deliver("charge.dispute.closed", "", stripe.Dispute{ID: disputeID, Charge: intent.LatestCharge,
		PaymentIntent: intent.ID, Amount: amount, Currency: intent.Currency, Status: status})
}

// UpdateAccount sets a connected account's flags and returns account.updated
// from the Connect endpoint.
func (f *Fake) UpdateAccount(id string, payoutsEnabled, detailsSubmitted bool, disabledReason string) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	account := f.accounts[id]
	account.PayoutsEnabled, account.ChargesEnabled, account.DetailsSubmitted = payoutsEnabled, payoutsEnabled, detailsSubmitted
	account.Requirements.DisabledReason = disabledReason
	f.accounts[id] = account
	return f.deliver("account.updated", id, account)
}

// ReverseTransfer reverses a transfer in full and returns transfer.reversed.
func (f *Fake) ReverseTransfer(id string) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	transfer := f.transfers[id]
	transfer.Reversed, transfer.AmountReversed = true, transfer.Amount
	f.transfers[id] = transfer
	return f.deliver("transfer.reversed", "", transfer)
}

// Raw signs an arbitrary event, for malformed-input tests.
func (f *Fake) Raw(kind string, object any) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deliver(kind, "", object)
}
