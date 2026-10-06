// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package stripe is the part of Stripe's API MOTOM uses: Checkout and
// PaymentIntents to sell credits, refunds, Connect Express accounts and
// account links to onboard creators, and transfers to pay them.
//
// By hand rather than through stripe-go, as MOQOM's billing does: a few
// form-encoded POSTs and one HMAC, against a dependency regenerated weekly
// from Stripe's whole API. A field Stripe renames reaches us as a test
// failure here rather than as a compile error there.
//
// Card data never passes through this package: buyers pay on Stripe-hosted
// Checkout or in Stripe Elements, and MOTOM sees only IDs and amounts.
package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client calls Stripe. The secret key comes from the environment (populated
// from Secret Manager) and is never logged.
type Client struct {
	SecretKey string
	// BaseURL is https://api.stripe.com unless a test says otherwise.
	BaseURL string
	HTTP    *http.Client
}

// Error is an error Stripe returned.
type Error struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("stripe %d %s %s: %s", e.Status, e.Type, e.Code, e.Message)
}

// Definitive reports whether err is Stripe refusing a request, as opposed to
// a failure that leaves its outcome unknown (a timeout, a 5xx, a rate limit,
// an idempotent request still in flight). An unknown outcome is retried with
// the same idempotency key; only a definitive one is unwound.
func Definitive(err error) bool {
	var stripeErr *Error
	if !errors.As(err, &stripeErr) {
		return false
	}
	return stripeErr.Status >= 400 && stripeErr.Status < 500 && stripeErr.Status != http.StatusConflict && stripeErr.Status != http.StatusTooManyRequests
}

func (c *Client) post(ctx context.Context, path string, form url.Values, idempotencyKey string, into any) error {
	if c == nil || c.SecretKey == "" {
		return ErrNotConfigured
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.SetBasicAuth(c.SecretKey, "")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("stripe %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe %s: reading the response: %w", path, err)
	}
	if response.StatusCode >= 300 {
		var envelope struct {
			Error Error `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		envelope.Error.Status = response.StatusCode
		return &envelope.Error
	}
	return json.Unmarshal(body, into)
}

// ErrNotConfigured is a deployment with no Stripe key.
var ErrNotConfigured = errors.New("stripe is not configured on this deployment")

func setMetadata(form url.Values, prefix string, metadata map[string]string) {
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		form.Set(prefix+"["+k+"]", metadata[k])
	}
}

// CheckoutParams describes a hosted payment page for one credit pack.
type CheckoutParams struct {
	Currency    string
	Amount      int64
	ProductName string
	SuccessURL  string
	CancelURL   string
	Reference   string
	// Metadata is written onto the payment intent, which is where the
	// webhook reads it back.
	Metadata map[string]string
}

// Checkout is a Checkout Session.
type Checkout struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateCheckout opens a Checkout Session.
func (c *Client) CreateCheckout(ctx context.Context, p CheckoutParams, idempotencyKey string) (Checkout, error) {
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("client_reference_id", p.Reference)
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", p.Currency)
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(p.Amount, 10))
	form.Set("line_items[0][price_data][product_data][name]", p.ProductName)
	setMetadata(form, "payment_intent_data[metadata]", p.Metadata)
	setMetadata(form, "metadata", p.Metadata)
	var checkout Checkout
	err := c.post(ctx, "/v1/checkout/sessions", form, idempotencyKey, &checkout)
	return checkout, err
}

// PaymentIntent is the subset of a payment intent MOTOM reads.
type PaymentIntent struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Amount         int64             `json:"amount"`
	AmountReceived int64             `json:"amount_received"`
	Currency       string            `json:"currency"`
	ClientSecret   string            `json:"client_secret"`
	LatestCharge   string            `json:"latest_charge"`
	Metadata       map[string]string `json:"metadata"`
}

// CreatePaymentIntent starts a payment the tenant's app confirms with Stripe
// Elements, so the card goes from the buyer's browser to Stripe directly.
func (c *Client) CreatePaymentIntent(ctx context.Context, amount int64, currency string, metadata map[string]string, idempotencyKey string) (PaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(amount, 10))
	form.Set("currency", currency)
	form.Set("automatic_payment_methods[enabled]", "true")
	setMetadata(form, "metadata", metadata)
	var intent PaymentIntent
	err := c.post(ctx, "/v1/payment_intents", form, idempotencyKey, &intent)
	return intent, err
}

// Refund is the subset of a refund MOTOM reads.
type Refund struct {
	ID            string `json:"id"`
	Amount        int64  `json:"amount"`
	Status        string `json:"status"`
	PaymentIntent string `json:"payment_intent"`
}

// CreateRefund refunds part or all of a payment intent.
func (c *Client) CreateRefund(ctx context.Context, intent string, amount int64, metadata map[string]string, idempotencyKey string) (Refund, error) {
	form := url.Values{}
	form.Set("payment_intent", intent)
	form.Set("amount", strconv.FormatInt(amount, 10))
	setMetadata(form, "metadata", metadata)
	var refund Refund
	err := c.post(ctx, "/v1/refunds", form, idempotencyKey, &refund)
	return refund, err
}

// Account is the subset of a connected account MOTOM reads.
type Account struct {
	ID               string `json:"id"`
	ChargesEnabled   bool   `json:"charges_enabled"`
	PayoutsEnabled   bool   `json:"payouts_enabled"`
	DetailsSubmitted bool   `json:"details_submitted"`
	Requirements     struct {
		DisabledReason string   `json:"disabled_reason"`
		CurrentlyDue   []string `json:"currently_due"`
	} `json:"requirements"`
	Metadata map[string]string `json:"metadata"`
}

// CreateExpressAccount makes the connected account a creator is paid into.
// Stripe collects and verifies the creator's identity (KYC); MOTOM stores
// only the account ID and the status flags Stripe reports.
func (c *Client) CreateExpressAccount(ctx context.Context, country string, metadata map[string]string, idempotencyKey string) (Account, error) {
	form := url.Values{}
	form.Set("type", "express")
	if country != "" {
		form.Set("country", country)
	}
	form.Set("capabilities[transfers][requested]", "true")
	setMetadata(form, "metadata", metadata)
	var account Account
	err := c.post(ctx, "/v1/accounts", form, idempotencyKey, &account)
	return account, err
}

// CreateAccountLink opens Stripe's hosted onboarding for a connected account.
func (c *Client) CreateAccountLink(ctx context.Context, account, refreshURL, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("account", account)
	form.Set("refresh_url", refreshURL)
	form.Set("return_url", returnURL)
	form.Set("type", "account_onboarding")
	var link struct {
		URL string `json:"url"`
	}
	err := c.post(ctx, "/v1/account_links", form, "", &link)
	return link.URL, err
}

// Transfer is the subset of a transfer MOTOM reads.
type Transfer struct {
	ID             string            `json:"id"`
	Amount         int64             `json:"amount"`
	AmountReversed int64             `json:"amount_reversed"`
	Currency       string            `json:"currency"`
	Destination    string            `json:"destination"`
	Reversed       bool              `json:"reversed"`
	Metadata       map[string]string `json:"metadata"`
}

// CreateTransfer moves money from MOTOM's Stripe balance to a connected
// account. Stripe then pays it out to the creator's bank on the connected
// account's schedule.
func (c *Client) CreateTransfer(ctx context.Context, amount int64, currency, destination, group string, metadata map[string]string, idempotencyKey string) (Transfer, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(amount, 10))
	form.Set("currency", currency)
	form.Set("destination", destination)
	if group != "" {
		form.Set("transfer_group", group)
	}
	setMetadata(form, "metadata", metadata)
	var transfer Transfer
	err := c.post(ctx, "/v1/transfers", form, idempotencyKey, &transfer)
	return transfer, err
}

// Charge is the subset of a charge MOTOM reads.
type Charge struct {
	ID             string `json:"id"`
	PaymentIntent  string `json:"payment_intent"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
}

// Dispute is the subset of a dispute MOTOM reads.
type Dispute struct {
	ID            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
}
