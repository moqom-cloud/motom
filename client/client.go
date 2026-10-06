// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package client calls the MOTOM API.
//
//	c := client.New("https://motom.example", os.Getenv("MOTOM_API_KEY"))
//	res, err := c.Tip(ctx, client.TipRequest{Room: "r1", Sender: "u1", Creator: "u2",
//		Amount: 500, Currency: "usd"}, client.NewIdempotencyKey())
//
// Amounts are integers in the currency's minor unit (cents for USD). Every
// call that moves money takes an idempotency key: retry a failed call with
// the same key and MOTOM applies it at most once.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is safe for concurrent use.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New returns a client for the deployment at baseURL.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// NewIdempotencyKey returns a random key. Keep it with the operation so a
// retry reuses it.
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is set on 429s.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("motom: %d %s: %s", e.Status, e.Code, e.Message)
}

// Codes in Error.Code.
const (
	CodeInvalid             = "invalid"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodeFrozen              = "frozen"
	CodeInsufficientFunds   = "insufficient_funds"
	CodeRateLimited         = "rate_limited"
	CodeDailyLimit          = "daily_limit"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeIdempotencyMismatch = "idempotency_mismatch"
	CodePaymentsUnavailable = "payments_unavailable"
	CodeStripeRefused       = "stripe_refused"
)

// IsCode reports whether err is an API error with the code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, idempotencyKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		e := &Error{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = time.Duration(seconds) * time.Second
		}
		return e
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func seg(s string) string { return url.PathEscape(s) }

func limitQuery(limit int) url.Values {
	if limit <= 0 {
		return nil
	}
	return url.Values{"limit": {strconv.Itoa(limit)}}
}

// Tenant returns the calling key's tenant. Needs admin.
func (c *Client) Tenant(ctx context.Context) (Tenant, error) {
	var t Tenant
	return t, c.do(ctx, http.MethodGet, "/v1/motom/tenant", nil, "", nil, &t)
}

// UpdateConfig replaces the tenant's configuration. Needs admin.
func (c *Client) UpdateConfig(ctx context.Context, config TenantConfig) (Tenant, error) {
	var t Tenant
	return t, c.do(ctx, http.MethodPut, "/v1/motom/tenant/config", nil, "", config, &t)
}

// Keys lists the tenant's API keys. Needs admin.
func (c *Client) Keys(ctx context.Context) ([]Key, error) {
	var out struct {
		Keys []Key `json:"keys"`
	}
	return out.Keys, c.do(ctx, http.MethodGet, "/v1/motom/keys", nil, "", nil, &out)
}

// IssueKey creates a key and returns it with its plaintext, which is shown
// only once. Needs admin.
func (c *Client) IssueKey(ctx context.Context, name string, scopes ...string) (Key, string, error) {
	var out struct {
		Key    Key    `json:"key"`
		APIKey string `json:"api_key"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/motom/keys", nil, "", map[string]any{"name": name, "scopes": scopes}, &out)
	return out.Key, out.APIKey, err
}

// RevokeKey revokes a key other than the caller's. Needs admin.
func (c *Client) RevokeKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/motom/keys/"+seg(id), nil, "", nil, nil)
}

// Packs lists the credit packs on sale. Needs wallets:read.
func (c *Client) Packs(ctx context.Context) ([]Pack, error) {
	var out struct {
		Packs []Pack `json:"packs"`
	}
	return out.Packs, c.do(ctx, http.MethodGet, "/v1/motom/packs", nil, "", nil, &out)
}

// Wallet returns a user's balances. Needs wallets:read.
func (c *Client) Wallet(ctx context.Context, user string) (Wallet, error) {
	var w Wallet
	return w, c.do(ctx, http.MethodGet, "/v1/motom/wallets/"+seg(user), nil, "", nil, &w)
}

// WalletEntries returns the newest ledger entries touching a wallet. Needs
// wallets:read.
func (c *Client) WalletEntries(ctx context.Context, user string, limit int) ([]Entry, error) {
	var out struct {
		Entries []Entry `json:"entries"`
	}
	return out.Entries, c.do(ctx, http.MethodGet, "/v1/motom/wallets/"+seg(user)+"/entries", limitQuery(limit), "", nil, &out)
}

// SpendRequest debits credits from a wallet for something the tenant sells.
type SpendRequest struct {
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency,omitempty"`
	Reference string `json:"reference,omitempty"`
	Memo      string `json:"memo,omitempty"`
}

// Spend debits a wallet. Needs wallets:write.
func (c *Client) Spend(ctx context.Context, user string, r SpendRequest, idempotencyKey string) (Entry, error) {
	var out struct {
		Entry Entry `json:"entry"`
	}
	return out.Entry, c.do(ctx, http.MethodPost, "/v1/motom/wallets/"+seg(user)+"/spend", nil, idempotencyKey, r, &out)
}

// GrantRequest gives a user credits the tenant pays for.
type GrantRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency,omitempty"`
	Memo     string `json:"memo,omitempty"`
}

// Grant credits a wallet from the tenant's account. Needs wallets:write.
func (c *Client) Grant(ctx context.Context, user string, r GrantRequest, idempotencyKey string) (Entry, error) {
	var out struct {
		Entry Entry `json:"entry"`
	}
	return out.Entry, c.do(ctx, http.MethodPost, "/v1/motom/wallets/"+seg(user)+"/grant", nil, idempotencyKey, r, &out)
}

// Freeze stops a wallet spending or tipping. Needs reviews:write.
func (c *Client) Freeze(ctx context.Context, user, reason string) (Wallet, error) {
	var w Wallet
	return w, c.do(ctx, http.MethodPost, "/v1/motom/wallets/"+seg(user)+"/freeze", nil, "", map[string]string{"reason": reason}, &w)
}

// Unfreeze lifts a freeze. Needs reviews:write.
func (c *Client) Unfreeze(ctx context.Context, user, reason string) (Wallet, error) {
	var w Wallet
	return w, c.do(ctx, http.MethodPost, "/v1/motom/wallets/"+seg(user)+"/unfreeze", nil, "", map[string]string{"reason": reason}, &w)
}

// Purchase modes.
const (
	ModeCheckout      = "checkout"
	ModePaymentIntent = "payment_intent"
)

// PurchaseRequest buys a credit pack.
type PurchaseRequest struct {
	User string `json:"user"`
	Pack string `json:"pack"`
	// Mode is ModeCheckout (redirect to CheckoutURL) or ModePaymentIntent
	// (confirm ClientSecret with Stripe Elements).
	Mode       string `json:"mode,omitempty"`
	SuccessURL string `json:"success_url,omitempty"`
	CancelURL  string `json:"cancel_url,omitempty"`
}

// Purchase starts a purchase. Credits land when Stripe confirms payment.
// Needs purchases:write.
func (c *Client) Purchase(ctx context.Context, r PurchaseRequest, idempotencyKey string) (Purchase, error) {
	var p Purchase
	return p, c.do(ctx, http.MethodPost, "/v1/motom/purchases", nil, idempotencyKey, r, &p)
}

// GetPurchase returns a purchase. Needs wallets:read.
func (c *Client) GetPurchase(ctx context.Context, id string) (Purchase, error) {
	var p Purchase
	return p, c.do(ctx, http.MethodGet, "/v1/motom/purchases/"+seg(id), nil, "", nil, &p)
}

// Refund refunds amount of a purchase, or the rest of it if amount is 0.
// Needs refunds:write.
func (c *Client) Refund(ctx context.Context, id string, amount int64, idempotencyKey string) (Purchase, Entry, error) {
	var out struct {
		Purchase Purchase `json:"purchase"`
		Entry    Entry    `json:"entry"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/motom/purchases/"+seg(id)+"/refund", nil, idempotencyKey, map[string]int64{"amount": amount}, &out)
	return out.Purchase, out.Entry, err
}

// TipRequest moves credits from a viewer to a creator in a room.
type TipRequest struct {
	Room     string `json:"room"`
	Sender   string `json:"sender"`
	Creator  string `json:"creator"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency,omitempty"`
	Message  string `json:"message,omitempty"`
}

// TipResult is a tip, its ledger entry and the event to show in the room.
type TipResult struct {
	Tip   Tip      `json:"tip"`
	Entry Entry    `json:"entry"`
	Event TipEvent `json:"event"`
	// Replayed is true when the idempotency key had already been used.
	Replayed bool `json:"replayed"`
}

// Tip sends a tip. Needs tips:write.
func (c *Client) Tip(ctx context.Context, r TipRequest, idempotencyKey string) (TipResult, error) {
	var out TipResult
	return out, c.do(ctx, http.MethodPost, "/v1/motom/tips", nil, idempotencyKey, r, &out)
}

// RoomTips returns the newest tips in a room. Needs tips:read.
func (c *Client) RoomTips(ctx context.Context, room string, limit int) ([]Tip, error) {
	var out struct {
		Tips []Tip `json:"tips"`
	}
	return out.Tips, c.do(ctx, http.MethodGet, "/v1/motom/rooms/"+seg(room)+"/tips", limitQuery(limit), "", nil, &out)
}

// Onboard returns a Stripe-hosted link where a creator verifies identity and
// adds a bank account. Call again for a fresh link. Needs payouts:write.
func (c *Client) Onboard(ctx context.Context, creator, country, refreshURL, returnURL string) (string, error) {
	var out struct {
		URL string `json:"onboarding_url"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/motom/creators/"+seg(creator)+"/onboarding", nil, "",
		map[string]string{"country": country, "refresh_url": refreshURL, "return_url": returnURL}, &out)
	return out.URL, err
}

// Creator returns a creator's payout status and earnings. Needs payouts:read.
func (c *Client) Creator(ctx context.Context, creator string) (Creator, error) {
	var out Creator
	return out, c.do(ctx, http.MethodGet, "/v1/motom/creators/"+seg(creator), nil, "", nil, &out)
}

// Payouts lists payouts, optionally for one creator or status. Needs
// payouts:read.
func (c *Client) Payouts(ctx context.Context, creator, status string, limit int) ([]Payout, error) {
	q := limitQuery(limit)
	if q == nil {
		q = url.Values{}
	}
	if creator != "" {
		q.Set("creator", creator)
	}
	if status != "" {
		q.Set("status", status)
	}
	var out struct {
		Payouts []Payout `json:"payouts"`
	}
	return out.Payouts, c.do(ctx, http.MethodGet, "/v1/motom/payouts", q, "", nil, &out)
}

// RunPayouts pays every payable creator. A run (default: today, UTC) pays
// each creator at most once, so rerunning is safe. Needs payouts:write.
func (c *Client) RunPayouts(ctx context.Context, run string) (RunReport, error) {
	var out RunReport
	return out, c.do(ctx, http.MethodPost, "/v1/motom/payouts/run", nil, "", map[string]string{"run": run}, &out)
}

// Reviews lists reviews by status: "open" (default), "approved", "rejected"
// or "all". Needs reviews:read.
func (c *Client) Reviews(ctx context.Context, status string) ([]Review, error) {
	var q url.Values
	if status != "" {
		q = url.Values{"status": {status}}
	}
	var out struct {
		Reviews []Review `json:"reviews"`
	}
	return out.Reviews, c.do(ctx, http.MethodGet, "/v1/motom/reviews", q, "", nil, &out)
}

// ResolveReview approves or rejects a review. Needs reviews:write.
func (c *Client) ResolveReview(ctx context.Context, id string, approve bool, note string) (Review, error) {
	decision := "reject"
	if approve {
		decision = "approve"
	}
	var out Review
	return out, c.do(ctx, http.MethodPost, "/v1/motom/reviews/"+seg(id)+"/resolve", nil, "",
		map[string]string{"decision": decision, "note": note}, &out)
}

// ExportUser returns everything MOTOM holds about a user. Needs dsar.
func (c *Client) ExportUser(ctx context.Context, user string) (UserExport, error) {
	var out UserExport
	return out, c.do(ctx, http.MethodGet, "/v1/motom/users/"+seg(user)+"/export", nil, "", nil, &out)
}

// EraseUser erases what can lawfully be erased and says what is retained.
// Needs dsar.
func (c *Client) EraseUser(ctx context.Context, user string) (Erasure, error) {
	var out Erasure
	return out, c.do(ctx, http.MethodPost, "/v1/motom/users/"+seg(user)+"/erase", nil, "", nil, &out)
}

// TrialBalance sums every posting per currency; balanced books are all zero.
// Needs admin.
func (c *Client) TrialBalance(ctx context.Context) (TrialBalance, error) {
	var out TrialBalance
	return out, c.do(ctx, http.MethodGet, "/v1/motom/ledger/trial-balance", nil, "", nil, &out)
}

// Audit returns the newest audit records, optionally for one subject such as
// "wallet:u1". Needs admin.
func (c *Client) Audit(ctx context.Context, subject string, limit int) ([]AuditRecord, error) {
	q := limitQuery(limit)
	if subject != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("subject", subject)
	}
	var out struct {
		Audit []AuditRecord `json:"audit"`
	}
	return out.Audit, c.do(ctx, http.MethodGet, "/v1/motom/audit", q, "", nil, &out)
}
