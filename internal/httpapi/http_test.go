// Copyright 2026 Elliott Wave LLC. All rights reserved.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/memory"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/internal/stripe/stripefake"
	"github.com/moqom-cloud/motom/ledger"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func setup(t *testing.T, opts Options) (http.Handler, *service.Service, *stripefake.Fake) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	fake := stripefake.New(now)
	t.Cleanup(fake.Server.Close)
	svc, err := service.New(service.Config{Store: memory.New(), Stripe: fake.Client(),
		WebhookSecrets: []string{fake.WebhookSecret}, Now: now, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTenant(context.Background(), "operator", "t_1", []ledger.Currency{"usd"}, 500); err != nil {
		t.Fatal(err)
	}
	opts.Logger, opts.Now = quiet, now
	return Handler(svc, opts), svc, fake
}

func issue(t *testing.T, svc *service.Service, scopes ...store.Scope) string {
	t.Helper()
	_, plaintext, err := svc.IssueKey(context.Background(), "operator", "t_1", "test", scopes)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func call(h http.Handler, method, path, key, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an error body: %s", rec.Body)
	}
	return body.Error.Code
}

func TestAuthentication(t *testing.T) {
	h, svc, _ := setup(t, Options{})
	key := issue(t, svc, store.ScopeWalletsRead)
	for name, credential := range map[string]string{
		"none":      "",
		"malformed": "not-a-key",
		"unknown":   "mtk_0123456789abcdef_" + strings.Repeat("A", 43),
		"wrong":     key[:len(key)-1] + "x",
	} {
		rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", credential, "")
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthorized" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", key, ""); rec.Code != http.StatusOK {
		t.Fatalf("good key: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", "", "", "Authorization", "bearer "+key); rec.Code != http.StatusOK {
		t.Fatalf("lower-case scheme: %d", rec.Code)
	}
	for _, route := range [][2]string{
		{http.MethodPost, "/v1/motom/tips"}, {http.MethodGet, "/v1/motom/tenant"}, {http.MethodPost, "/v1/motom/payouts/run"},
		{http.MethodGet, "/v1/motom/users/u_1/export"}, {http.MethodPost, "/v1/motom/wallets/u_1/freeze"},
	} {
		if rec := call(h, route[0], route[1], key, "{}"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with wallets:read: %d", route[0], route[1], rec.Code)
		}
	}
}

func TestAdminCannotRevokeItsOwnKey(t *testing.T) {
	h, svc, _ := setup(t, Options{})
	key := issue(t, svc, store.ScopeAdmin)
	id, _, _ := service.ParseKey(key)
	if rec := call(h, http.MethodDelete, "/v1/motom/keys/"+id, key, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("self-revoke: %d", rec.Code)
	}
	rec := call(h, http.MethodPost, "/v1/motom/keys", key, `{"name":"app","scopes":["tips:write"]}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"api_key":"mtk_`) {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
}

func TestRequestsAreStrict(t *testing.T) {
	h, svc, _ := setup(t, Options{})
	key := issue(t, svc, store.ScopeWalletsWrite)
	body := `{"amount":100,"currency":"usd"}`
	if rec := call(h, http.MethodPost, "/v1/motom/wallets/u_1/grant", key, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("no idempotency key: %d", rec.Code)
	}
	rec := call(h, http.MethodPost, "/v1/motom/wallets/u_1/grant", key, `{"amount":100,"bogus":1}`, "Idempotency-Key", "k1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	huge := `{"memo":"` + strings.Repeat("x", largestBody) + `"}`
	if rec := call(h, http.MethodPost, "/v1/motom/wallets/u_1/grant", key, huge, "Idempotency-Key", "k1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", rec.Code)
	}
	if rec := call(h, http.MethodPost, "/v1/motom/wallets/u_1/grant", key, body, "Idempotency-Key", "k1"); rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, http.MethodPost, "/v1/motom/wallets/u_1/grant", key, `{"amount":101,"currency":"usd"}`, "Idempotency-Key", "k1")
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "idempotency_mismatch" {
		t.Fatalf("reused key: %d %s", rec.Code, rec.Body)
	}
}

func TestPerKeyRateLimit(t *testing.T) {
	h, svc, _ := setup(t, Options{RequestsPerMinute: 3})
	key, other := issue(t, svc, store.ScopeWalletsRead), issue(t, svc, store.ScopeWalletsRead)
	for i := 0; i < 3; i++ {
		if rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", key, ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", key, "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over limit: %d", rec.Code)
	}
	if rec := call(h, http.MethodGet, "/v1/motom/wallets/u_1", other, ""); rec.Code != http.StatusOK {
		t.Fatalf("another key: %d", rec.Code)
	}
}

func TestLimiterForgetsIdleKeys(t *testing.T) {
	at := time.Unix(0, 0)
	l := &windowLimiter{per: 1, window: time.Minute, now: func() time.Time { return at }}
	for i := 0; i < sweepAbove; i++ {
		l.permit(fmt.Sprint(i))
	}
	at = at.Add(time.Minute)
	if !l.permit("0") || len(l.seen) != 1 {
		t.Fatalf("%d keys tracked after the window passed", len(l.seen))
	}
}

func TestWebhookEndpoint(t *testing.T) {
	h, _, fake := setup(t, Options{})
	delivery := fake.Raw("customer.created", map[string]string{"id": "cus_1"})
	webhook := func(payload, signature string) int {
		return call(h, http.MethodPost, "/v1/motom/webhooks/stripe", "", payload, "Stripe-Signature", signature).Code
	}
	if code := webhook(string(delivery.Payload), delivery.Header); code != http.StatusOK {
		t.Fatalf("signed: %d", code)
	}
	if code := webhook(string(delivery.Payload)+" ", delivery.Header); code != http.StatusBadRequest {
		t.Fatalf("altered payload: %d", code)
	}
	if code := webhook(string(delivery.Payload), ""); code != http.StatusBadRequest {
		t.Fatalf("unsigned: %d", code)
	}
}

func TestErrorsDoNotLeak(t *testing.T) {
	h := &handler{log: quiet}
	for err, want := range map[error]int{
		service.ErrInvalid:                         http.StatusBadRequest,
		service.ErrInsufficientFunds:               http.StatusPaymentRequired,
		service.ErrFrozen:                          http.StatusForbidden,
		service.ErrLimitExceeded:                   http.StatusTooManyRequests,
		store.ErrNotFound:                          http.StatusNotFound,
		service.ErrConflict:                        http.StatusConflict,
		service.ErrPaymentsUnavailable:             http.StatusServiceUnavailable,
		&stripe.Error{Status: 400, Code: "x"}:      http.StatusBadGateway,
		errors.New("pq: password=hunter2 refused"): http.StatusInternalServerError,
	} {
		rec := httptest.NewRecorder()
		h.fail(rec, httptest.NewRequest(http.MethodGet, "/", nil), err)
		if rec.Code != want {
			t.Errorf("%v: %d, want %d", err, rec.Code, want)
		}
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("internal error leaked: %s", rec.Body)
		}
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	h, _, _ := setup(t, Options{})
	rec := call(h, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}
