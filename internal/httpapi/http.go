// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package httpapi serves MOTOM over HTTPS under /v1/motom.
//
// Authentication is a tenant API key (mtk_<id>_<secret>) as a bearer token,
// resolved in constant time against a stored SHA-256, and every route names
// the scope it needs. Every request that moves money requires an
// Idempotency-Key header; replaying one returns the original result.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

// Options tunes the handler.
type Options struct {
	// RequestsPerMinute bounds each API key. Zero is 600.
	RequestsPerMinute int
	Logger            *slog.Logger
	Now               func() time.Time
}

// Handler serves the API.
//
//	POST   /v1/motom/webhooks/stripe                 Stripe (signature-verified)
//	GET    /v1/motom/tenant                          admin
//	PUT    /v1/motom/tenant/config                   admin
//	GET    /v1/motom/keys                            admin
//	POST   /v1/motom/keys                            admin
//	DELETE /v1/motom/keys/{id}                       admin
//	GET    /v1/motom/packs                           wallets:read
//	GET    /v1/motom/wallets/{user}                  wallets:read
//	GET    /v1/motom/wallets/{user}/entries          wallets:read
//	POST   /v1/motom/wallets/{user}/spend            wallets:write    Idempotency-Key
//	POST   /v1/motom/wallets/{user}/grant            wallets:write    Idempotency-Key
//	POST   /v1/motom/wallets/{user}/freeze           reviews:write
//	POST   /v1/motom/wallets/{user}/unfreeze         reviews:write
//	POST   /v1/motom/purchases                       purchases:write  Idempotency-Key
//	GET    /v1/motom/purchases/{id}                  wallets:read
//	POST   /v1/motom/purchases/{id}/refund           refunds:write    Idempotency-Key
//	POST   /v1/motom/tips                            tips:write       Idempotency-Key
//	GET    /v1/motom/rooms/{room}/tips               tips:read
//	POST   /v1/motom/creators/{creator}/onboarding   payouts:write
//	GET    /v1/motom/creators/{creator}              payouts:read
//	GET    /v1/motom/payouts                         payouts:read
//	POST   /v1/motom/payouts/run                     payouts:write
//	GET    /v1/motom/reviews                         reviews:read
//	POST   /v1/motom/reviews/{id}/resolve            reviews:write
//	GET    /v1/motom/users/{user}/export             dsar
//	POST   /v1/motom/users/{user}/erase              dsar
//	GET    /v1/motom/ledger/trial-balance            admin
//	GET    /v1/motom/audit                           admin
//	GET    /healthz
func Handler(svc *service.Service, opts Options) http.Handler {
	if opts.RequestsPerMinute == 0 {
		opts.RequestsPerMinute = 600
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := &handler{svc: svc, log: opts.Logger,
		limiter: &windowLimiter{per: opts.RequestsPerMinute, window: time.Minute, now: opts.Now}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /v1/motom/webhooks/stripe", h.webhook)
	route := func(pattern string, scope store.Scope, next func(http.ResponseWriter, *http.Request, store.Key)) {
		mux.HandleFunc(pattern, h.authed(scope, next))
	}
	route("GET /v1/motom/tenant", store.ScopeAdmin, h.tenant)
	route("PUT /v1/motom/tenant/config", store.ScopeAdmin, h.config)
	route("GET /v1/motom/keys", store.ScopeAdmin, h.listKeys)
	route("POST /v1/motom/keys", store.ScopeAdmin, h.issueKey)
	route("DELETE /v1/motom/keys/{id}", store.ScopeAdmin, h.revokeKey)
	route("GET /v1/motom/packs", store.ScopeWalletsRead, h.packs)
	route("GET /v1/motom/wallets/{user}", store.ScopeWalletsRead, h.wallet)
	route("GET /v1/motom/wallets/{user}/entries", store.ScopeWalletsRead, h.walletEntries)
	route("POST /v1/motom/wallets/{user}/spend", store.ScopeWalletsWrite, h.spend)
	route("POST /v1/motom/wallets/{user}/grant", store.ScopeWalletsWrite, h.grant)
	route("POST /v1/motom/wallets/{user}/freeze", store.ScopeReviewsWrite, h.freeze(true))
	route("POST /v1/motom/wallets/{user}/unfreeze", store.ScopeReviewsWrite, h.freeze(false))
	route("POST /v1/motom/purchases", store.ScopePurchasesWrite, h.purchase)
	route("GET /v1/motom/purchases/{id}", store.ScopeWalletsRead, h.getPurchase)
	route("POST /v1/motom/purchases/{id}/refund", store.ScopeRefundsWrite, h.refund)
	route("POST /v1/motom/tips", store.ScopeTipsWrite, h.tip)
	route("GET /v1/motom/rooms/{room}/tips", store.ScopeTipsRead, h.roomTips)
	route("POST /v1/motom/creators/{creator}/onboarding", store.ScopePayoutsWrite, h.onboard)
	route("GET /v1/motom/creators/{creator}", store.ScopePayoutsRead, h.creator)
	route("GET /v1/motom/payouts", store.ScopePayoutsRead, h.payouts)
	route("POST /v1/motom/payouts/run", store.ScopePayoutsWrite, h.runPayouts)
	route("GET /v1/motom/reviews", store.ScopeReviewsRead, h.reviews)
	route("POST /v1/motom/reviews/{id}/resolve", store.ScopeReviewsWrite, h.resolve)
	route("GET /v1/motom/users/{user}/export", store.ScopeDSAR, h.export)
	route("POST /v1/motom/users/{user}/erase", store.ScopeDSAR, h.erase)
	route("GET /v1/motom/ledger/trial-balance", store.ScopeAdmin, h.trialBalance)
	route("GET /v1/motom/audit", store.ScopeAdmin, h.audit)
	return securityHeaders(mux)
}

type handler struct {
	svc     *service.Service
	log     *slog.Logger
	limiter *windowLimiter
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

const largestBody = 64 << 10

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var stripeErr *stripe.Error
	switch {
	case errors.Is(err, service.ErrInvalidKey):
		status, code = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, service.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, service.ErrFrozen):
		status, code = http.StatusForbidden, "frozen"
	case errors.Is(err, store.ErrIdempotencyMismatch):
		status, code = http.StatusUnprocessableEntity, "idempotency_mismatch"
	case errors.Is(err, service.ErrInvalid), errors.Is(err, ledger.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid"
	case errors.Is(err, service.ErrInsufficientFunds):
		status, code = http.StatusPaymentRequired, "insufficient_funds"
	case errors.Is(err, service.ErrRateLimited):
		status, code = http.StatusTooManyRequests, "rate_limited"
		w.Header().Set("Retry-After", "60")
	case errors.Is(err, service.ErrLimitExceeded):
		status, code = http.StatusTooManyRequests, "daily_limit"
	case errors.Is(err, store.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, store.ErrAlreadyExists):
		status, code = http.StatusConflict, "already_exists"
	case errors.Is(err, service.ErrConflict), errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrAlreadyReversed):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, service.ErrPaymentsUnavailable), errors.Is(err, stripe.ErrNotConfigured):
		status, code = http.StatusServiceUnavailable, "payments_unavailable"
	case errors.Is(err, service.ErrStripeRefused):
		status, code = http.StatusBadGateway, "stripe_refused"
	case errors.As(err, &stripeErr):
		status, code = http.StatusBadGateway, "stripe_error"
	}
	message := err.Error()
	if status == http.StatusInternalServerError {
		h.log.ErrorContext(r.Context(), "request failed", "event", "motom_request_failed", "path", r.URL.Path, "error", err)
		message = "internal error"
	}
	if status == http.StatusBadGateway {
		// Stripe's message can name internal IDs; the code is enough.
		h.log.WarnContext(r.Context(), "stripe error", "event", "motom_stripe_error", "path", r.URL.Path, "error", err)
		message = "the payment provider refused or failed the request"
	}
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

func decode(w http.ResponseWriter, r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, largestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.Join(service.ErrInvalid, err)
	}
	return nil
}

func bearer(r *http.Request) string {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

func actor(k store.Key) string { return "key:" + k.ID }

func idempotencyKey(r *http.Request) string { return r.Header.Get("Idempotency-Key") }

func (h *handler) authed(scope store.Scope, next func(http.ResponseWriter, *http.Request, store.Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := h.svc.ResolveKey(r.Context(), bearer(r))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if !h.limiter.permit(key.ID) {
			h.fail(w, r, service.ErrRateLimited)
			return
		}
		if !key.Allows(scope) {
			h.fail(w, r, service.ErrForbidden)
			return
		}
		next(w, r, key)
	}
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *handler) webhook(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		h.fail(w, r, errors.Join(service.ErrInvalid, err))
		return
	}
	err = h.svc.HandleWebhook(r.Context(), payload, r.Header.Get("Stripe-Signature"))
	if errors.Is(err, service.ErrSignature) {
		h.log.WarnContext(r.Context(), "stripe webhook signature rejected", "event", "motom_webhook_signature_rejected", "remote", clientAddress(r))
		writeJSON(w, http.StatusBadRequest, map[string]apiError{"error": {Code: "signature", Message: "signature"}})
		return
	}
	if err != nil {
		// 500 makes Stripe retry with backoff; every handler is idempotent.
		h.log.ErrorContext(r.Context(), "stripe webhook failed", "event", "motom_webhook_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]apiError{"error": {Code: "internal", Message: "internal error"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}

func (h *handler) tenant(w http.ResponseWriter, r *http.Request, k store.Key) {
	t, err := h.svc.Tenant(r.Context(), k.Tenant)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTenant(t))
}

func (h *handler) config(w http.ResponseWriter, r *http.Request, k store.Key) {
	var config store.TenantConfig
	if err := decode(w, r, &config); err != nil {
		h.fail(w, r, err)
		return
	}
	t, err := h.svc.UpdateConfig(r.Context(), actor(k), k.Tenant, config)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTenant(t))
}

func (h *handler) listKeys(w http.ResponseWriter, r *http.Request, k store.Key) {
	keys, err := h.svc.ListKeys(r.Context(), k.Tenant)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, viewKey(key))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": views})
}

func (h *handler) issueKey(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Name   string        `json:"name"`
		Scopes []store.Scope `json:"scopes"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	key, plaintext, err := h.svc.IssueKey(r.Context(), actor(k), k.Tenant, body.Name, body.Scopes)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": viewKey(key), "api_key": plaintext,
		"note": "The API key is shown once. Store it in a secret manager now; it cannot be recovered, only revoked."})
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request, k store.Key) {
	id := r.PathValue("id")
	if id == k.ID {
		h.fail(w, r, errors.Join(service.ErrInvalid, errors.New("issue a replacement and revoke this key with it")))
		return
	}
	if err := h.svc.RevokeKey(r.Context(), actor(k), k.Tenant, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) packs(w http.ResponseWriter, r *http.Request, k store.Key) {
	t, err := h.svc.Tenant(r.Context(), k.Tenant)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	packs := t.Config.Packs
	if packs == nil {
		packs = []store.Pack{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"packs": packs})
}

func (h *handler) wallet(w http.ResponseWriter, r *http.Request, k store.Key) {
	state, err := h.svc.Wallet(r.Context(), k.Tenant, r.PathValue("user"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewWallet(state))
}

func limit(r *http.Request, fallback, most int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return fallback
	}
	return min(n, most)
}

func (h *handler) walletEntries(w http.ResponseWriter, r *http.Request, k store.Key) {
	entries, err := h.svc.WalletEntries(r.Context(), k.Tenant, r.PathValue("user"), limit(r, 50, 500))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": viewEntries(entries)})
}

func (h *handler) spend(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
		Reference string `json:"reference"`
		Memo      string `json:"memo"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	e, err := h.svc.Spend(r.Context(), actor(k), k.Tenant, service.SpendRequest{User: r.PathValue("user"),
		Amount: ledger.Amount(body.Amount), Currency: ledger.Currency(body.Currency), Reference: body.Reference,
		Memo: body.Memo, IdempotencyKey: idempotencyKey(r)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": viewEntry(e)})
}

func (h *handler) grant(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Memo     string `json:"memo"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	e, err := h.svc.Grant(r.Context(), actor(k), k.Tenant, service.GrantRequest{User: r.PathValue("user"),
		Amount: ledger.Amount(body.Amount), Currency: ledger.Currency(body.Currency), Memo: body.Memo, IdempotencyKey: idempotencyKey(r)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": viewEntry(e)})
}

func (h *handler) freeze(frozen bool) func(http.ResponseWriter, *http.Request, store.Key) {
	return func(w http.ResponseWriter, r *http.Request, k store.Key) {
		var body struct {
			Reason string `json:"reason"`
		}
		if r.ContentLength != 0 {
			if err := decode(w, r, &body); err != nil {
				h.fail(w, r, err)
				return
			}
		}
		if _, err := h.svc.SetFrozen(r.Context(), actor(k), k.Tenant, r.PathValue("user"), frozen, body.Reason); err != nil {
			h.fail(w, r, err)
			return
		}
		h.wallet(w, r, k)
	}
}

func (h *handler) purchase(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		User       string `json:"user"`
		Pack       string `json:"pack"`
		Mode       string `json:"mode"`
		SuccessURL string `json:"success_url"`
		CancelURL  string `json:"cancel_url"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	result, err := h.svc.Purchase(r.Context(), actor(k), k.Tenant, service.PurchaseRequest{User: body.User, Pack: body.Pack,
		Mode: body.Mode, SuccessURL: body.SuccessURL, CancelURL: body.CancelURL, IdempotencyKey: idempotencyKey(r)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	view := viewPurchase(result.Purchase)
	view.CheckoutURL, view.ClientSecret = result.CheckoutURL, result.ClientSecret
	writeJSON(w, http.StatusCreated, view)
}

func (h *handler) getPurchase(w http.ResponseWriter, r *http.Request, k store.Key) {
	p, err := h.svc.GetPurchase(r.Context(), k.Tenant, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewPurchase(p))
}

func (h *handler) refund(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Amount int64 `json:"amount"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &body); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	p, e, err := h.svc.Refund(r.Context(), actor(k), k.Tenant, r.PathValue("id"), ledger.Amount(body.Amount), idempotencyKey(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"purchase": viewPurchase(p), "entry": viewEntry(e)})
}

func (h *handler) tip(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Room     string `json:"room"`
		Sender   string `json:"sender"`
		Creator  string `json:"creator"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Message  string `json:"message"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	result, err := h.svc.Tip(r.Context(), actor(k), k.Tenant, service.TipRequest{Room: body.Room, Sender: body.Sender,
		Creator: body.Creator, Amount: ledger.Amount(body.Amount), Currency: ledger.Currency(body.Currency),
		Message: body.Message, IdempotencyKey: idempotencyKey(r)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"tip": viewTip(result.Tip), "entry": viewEntry(result.Entry),
		"event": result.Event, "replayed": result.Replayed})
}

func (h *handler) roomTips(w http.ResponseWriter, r *http.Request, k store.Key) {
	tips, err := h.svc.Tips(r.Context(), k.Tenant, store.TipQuery{Room: r.PathValue("room"), Limit: limit(r, 50, 500)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tips": viewTips(tips)})
}

func (h *handler) onboard(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Country    string `json:"country"`
		RefreshURL string `json:"refresh_url"`
		ReturnURL  string `json:"return_url"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	link, _, err := h.svc.Onboard(r.Context(), actor(k), k.Tenant, r.PathValue("creator"), body.Country, body.RefreshURL, body.ReturnURL)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"onboarding_url": link})
}

func (h *handler) creator(w http.ResponseWriter, r *http.Request, k store.Key) {
	state, err := h.svc.Payee(r.Context(), k.Tenant, r.PathValue("creator"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewCreator(state))
}

func (h *handler) payouts(w http.ResponseWriter, r *http.Request, k store.Key) {
	q := r.URL.Query()
	payouts, err := h.svc.Payouts(r.Context(), store.PayoutQuery{Tenant: k.Tenant, Creator: q.Get("creator"),
		Status: store.PayoutStatus(q.Get("status")), Limit: limit(r, 100, 1000)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": viewPayouts(payouts)})
}

func (h *handler) runPayouts(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Run string `json:"run"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &body); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	report, err := h.svc.RunPayouts(r.Context(), actor(k), k.Tenant, body.Run)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *handler) reviews(w http.ResponseWriter, r *http.Request, k store.Key) {
	status := store.ReviewStatus(r.URL.Query().Get("status"))
	if status == "" {
		status = store.ReviewOpen
	}
	if status == "all" {
		status = ""
	}
	reviews, err := h.svc.Reviews(r.Context(), k.Tenant, status)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": viewReviews(reviews)})
}

func (h *handler) resolve(w http.ResponseWriter, r *http.Request, k store.Key) {
	var body struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.Decision != "approve" && body.Decision != "reject" {
		h.fail(w, r, errors.Join(service.ErrInvalid, errors.New(`decision is "approve" or "reject"`)))
		return
	}
	review, err := h.svc.ResolveReview(r.Context(), actor(k), k.Tenant, r.PathValue("id"), body.Decision == "approve", body.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewReview(review))
}

func (h *handler) export(w http.ResponseWriter, r *http.Request, k store.Key) {
	export, err := h.svc.ExportUser(r.Context(), k.Tenant, r.PathValue("user"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	view := exportView{GeneratedAt: export.GeneratedAt, Tenant: export.Tenant, User: export.User,
		Wallet: viewWallet(export.Wallet), Entries: viewEntries(export.Entries), Purchases: []purchaseView{},
		TipsSent: viewTips(export.TipsSent), TipsReceived: viewTips(export.TipsReceived),
		Payouts: viewPayouts(export.Payouts), Reviews: viewReviews(export.Reviews),
		HeldBy: []string{"Stripe holds card details, payment records and, for creators, identity verification data; request them from Stripe."}}
	for _, p := range export.Purchases {
		view.Purchases = append(view.Purchases, viewPurchase(p))
	}
	if export.Payee != nil {
		state, err := h.svc.Payee(r.Context(), k.Tenant, export.User)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		creator := viewCreator(state)
		view.Creator = &creator
	}
	w.Header().Set("Content-Disposition", `attachment; filename="motom-user-export.json"`)
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) erase(w http.ResponseWriter, r *http.Request, k store.Key) {
	result, err := h.svc.EraseUser(r.Context(), actor(k), k.Tenant, r.PathValue("user"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tip_messages_erased": result.TipMessagesErased, "retained": result.Retained})
}

func (h *handler) trialBalance(w http.ResponseWriter, r *http.Request, k store.Key) {
	trial, err := h.svc.TrialBalance(r.Context(), k.Tenant)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	balanced := true
	sums := map[string]int64{}
	for c, sum := range trial {
		sums[string(c)] = int64(sum)
		balanced = balanced && sum == 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"balanced": balanced, "sums": sums})
}

func (h *handler) audit(w http.ResponseWriter, r *http.Request, k store.Key) {
	records, err := h.svc.Audit(r.Context(), k.Tenant, r.URL.Query().Get("subject"), limit(r, 100, 1000))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]auditView, 0, len(records))
	for _, a := range records {
		views = append(views, auditView{ID: a.ID, Actor: a.Actor, Action: a.Action, Subject: a.Subject, Detail: a.Detail, At: a.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": views})
}

const sweepAbove = 4096

// windowLimiter allows per events per window for each key.
type windowLimiter struct {
	mu     sync.Mutex
	per    int
	window time.Duration
	now    func() time.Time
	seen   map[string][]time.Time
}

func (l *windowLimiter) permit(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	now := l.now()
	if len(l.seen) >= sweepAbove {
		for other, times := range l.seen {
			if len(times) == 0 || now.Sub(times[len(times)-1]) >= l.window {
				delete(l.seen, other)
			}
		}
	}
	recent := l.seen[key][:0]
	for _, at := range l.seen[key] {
		if now.Sub(at) < l.window {
			recent = append(recent, at)
		}
	}
	if len(recent) >= l.per {
		l.seen[key] = recent
		return false
	}
	l.seen[key] = append(recent, now)
	return true
}
