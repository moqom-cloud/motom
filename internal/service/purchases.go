// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

// Purchase modes.
const (
	ModeCheckout      = "checkout"
	ModePaymentIntent = "payment_intent"
)

// PurchaseRequest buys a credit pack.
type PurchaseRequest struct {
	User string
	Pack string
	// Mode is ModeCheckout (a Stripe-hosted page) or ModePaymentIntent
	// (the tenant's app confirms with Stripe Elements).
	Mode           string
	SuccessURL     string
	CancelURL      string
	IdempotencyKey string
}

// PurchaseResult is a purchase awaiting payment.
type PurchaseResult struct {
	Purchase     store.Purchase
	CheckoutURL  string
	ClientSecret string
}

func returnURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return fmt.Errorf("%w: return URLs must be https", ErrInvalid)
	}
	return nil
}

// Purchase opens a payment for a pack. Credits arrive when Stripe's
// payment_intent.succeeded webhook does, never on the client's say-so.
func (s *Service) Purchase(ctx context.Context, actor, tenant string, r PurchaseRequest) (PurchaseResult, error) {
	if s.stripe == nil {
		return PurchaseResult{}, ErrPaymentsUnavailable
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return PurchaseResult{}, err
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return PurchaseResult{}, err
	}
	p, ok := pack(t, r.Pack)
	if !ok {
		return PurchaseResult{}, fmt.Errorf("%w: no pack %q", ErrInvalid, r.Pack)
	}
	switch r.Mode {
	case ModeCheckout:
		if err := returnURL(r.SuccessURL); err != nil {
			return PurchaseResult{}, err
		}
		if err := returnURL(r.CancelURL); err != nil {
			return PurchaseResult{}, err
		}
	case ModePaymentIntent:
	default:
		return PurchaseResult{}, fmt.Errorf("%w: mode is checkout or payment_intent", ErrInvalid)
	}
	w, err := s.ensureWallet(ctx, tenant, r.User)
	if err != nil {
		return PurchaseResult{}, err
	}
	if w.Frozen {
		return PurchaseResult{}, ErrFrozen
	}
	id := derivedID("pur", tenant, r.IdempotencyKey)
	purchase, err := s.store.GetPurchase(ctx, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		count, err := s.store.CountPurchases(ctx, tenant, r.User, s.now().Add(-time.Hour))
		if err != nil {
			return PurchaseResult{}, err
		}
		if count >= t.Config.PurchasesPerHour {
			s.audit(ctx, tenant, actor, "fraud.purchase_velocity", "wallet:"+r.User, strconv.Itoa(count))
			return PurchaseResult{}, fmt.Errorf("%w: at most %d purchases an hour", ErrRateLimited, t.Config.PurchasesPerHour)
		}
		now := s.now()
		purchase = store.Purchase{ID: id, Tenant: tenant, User: r.User, Pack: p.ID, Currency: p.Currency,
			Price: p.Price, Credits: p.Price + p.Bonus, Status: store.PurchasePending, Mode: r.Mode, CreatedAt: now, UpdatedAt: now}
		if err := s.store.CreatePurchase(ctx, purchase); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return PurchaseResult{}, err
		}
		if purchase, err = s.store.GetPurchase(ctx, tenant, id); err != nil {
			return PurchaseResult{}, err
		}
	} else if err != nil {
		return PurchaseResult{}, err
	}
	if purchase.User != r.User || purchase.Pack != r.Pack || purchase.Mode != r.Mode {
		return PurchaseResult{}, store.ErrIdempotencyMismatch
	}
	metadata := map[string]string{"motom_tenant": tenant, "motom_purchase": purchase.ID, "motom_user": r.User}
	// Stripe's own idempotency makes a replay return the session or intent
	// the first attempt created.
	result := PurchaseResult{Purchase: purchase}
	if r.Mode == ModeCheckout {
		checkout, err := s.stripe.CreateCheckout(ctx, stripe.CheckoutParams{
			Currency: string(p.Currency), Amount: int64(p.Price), ProductName: p.Name, SuccessURL: r.SuccessURL,
			CancelURL: r.CancelURL, Reference: purchase.ID, Metadata: metadata}, "motom:"+purchase.ID)
		if err != nil {
			return PurchaseResult{}, s.purchaseFailed(ctx, purchase, err)
		}
		result.CheckoutURL = checkout.URL
		if purchase.StripeCheckout == "" {
			purchase.StripeCheckout, purchase.UpdatedAt = checkout.ID, s.now()
			if err := s.store.SavePurchase(ctx, purchase); err != nil && !errors.Is(err, store.ErrVersionConflict) {
				return PurchaseResult{}, err
			}
		}
	} else {
		intent, err := s.stripe.CreatePaymentIntent(ctx, int64(p.Price), string(p.Currency), metadata, "motom:"+purchase.ID)
		if err != nil {
			return PurchaseResult{}, s.purchaseFailed(ctx, purchase, err)
		}
		result.ClientSecret = intent.ClientSecret
		if purchase.StripeIntent == "" {
			purchase.StripeIntent, purchase.UpdatedAt = intent.ID, s.now()
			if err := s.store.SavePurchase(ctx, purchase); err != nil && !errors.Is(err, store.ErrVersionConflict) {
				return PurchaseResult{}, err
			}
		}
	}
	if result.Purchase, err = s.store.GetPurchase(ctx, tenant, purchase.ID); err != nil {
		return PurchaseResult{}, err
	}
	return result, nil
}

func (s *Service) purchaseFailed(ctx context.Context, p store.Purchase, cause error) error {
	if stripe.Definitive(cause) {
		p.Status, p.UpdatedAt = store.PurchaseFailed, s.now()
		_ = s.store.SavePurchase(ctx, p)
	}
	return cause
}

// GetPurchase returns a purchase.
func (s *Service) GetPurchase(ctx context.Context, tenant, id string) (store.Purchase, error) {
	return s.store.GetPurchase(ctx, tenant, id)
}

// updatePurchase applies change to the latest copy until it saves.
func (s *Service) updatePurchase(ctx context.Context, tenant, id string, change func(*store.Purchase) bool) (store.Purchase, error) {
	for attempt := 0; attempt < 8; attempt++ {
		p, err := s.store.GetPurchase(ctx, tenant, id)
		if err != nil {
			return store.Purchase{}, err
		}
		if !change(&p) {
			return p, nil
		}
		p.UpdatedAt = s.now()
		err = s.store.SavePurchase(ctx, p)
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return store.Purchase{}, err
		}
		return s.store.GetPurchase(ctx, tenant, id)
	}
	return store.Purchase{}, ErrConflict
}

// purchaseFor finds the purchase a Stripe object belongs to. Objects with
// no MOTOM metadata and no known intent are not MOTOM's (the same Stripe
// account may also carry MOQOM Cloud billing) and are ignored.
func (s *Service) purchaseFor(ctx context.Context, intent string, metadata map[string]string) (store.Purchase, bool, error) {
	if tenant, id := metadata["motom_tenant"], metadata["motom_purchase"]; tenant != "" && id != "" {
		p, err := s.store.GetPurchase(ctx, tenant, id)
		if err == nil {
			return p, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.Purchase{}, false, err
		}
	}
	p, err := s.store.FindPurchaseByIntent(ctx, intent)
	if errors.Is(err, store.ErrNotFound) {
		return store.Purchase{}, false, nil
	}
	return p, err == nil, err
}

func (s *Service) paymentSucceeded(ctx context.Context, intent stripe.PaymentIntent) error {
	p, ours, err := s.purchaseFor(ctx, intent.ID, intent.Metadata)
	if err != nil || !ours {
		return err
	}
	if p.StripeIntent != "" && p.StripeIntent != intent.ID {
		return fmt.Errorf("purchase %s is paid by %s, not %s", p.ID, p.StripeIntent, intent.ID)
	}
	if intent.AmountReceived != int64(p.Price) || ledger.Currency(intent.Currency) != p.Currency {
		// Paid, but not what was sold. Credit nothing and put a person on it.
		_, err := s.updatePurchase(ctx, p.Tenant, p.ID, func(p *store.Purchase) bool {
			if p.Status == store.PurchaseFlagged {
				return false
			}
			p.Status, p.StripeIntent = store.PurchaseFlagged, intent.ID
			return true
		})
		if err != nil {
			return err
		}
		_, err = s.flag(ctx, p.Tenant, "purchase:"+p.ID, "amount_mismatch",
			fmt.Sprintf("received %d %s for a %d %s pack", intent.AmountReceived, intent.Currency, p.Price, p.Currency), intent.ID)
		return err
	}
	ids, err := s.accounts(ctx, p.Tenant, p.Currency, accountSpec{ledger.StripeClearing, ""},
		accountSpec{ledger.Wallet, p.User}, accountSpec{ledger.Tenant, ""})
	if err != nil {
		return err
	}
	if _, err := s.ensureWallet(ctx, p.Tenant, p.User); err != nil {
		return err
	}
	// The buyer pays Price; the wallet gets Price plus the pack's bonus,
	// which the tenant funds.
	lines := (&postings{}).add(ids[0], p.Price).add(ids[1], -p.Credits).add(ids[2], p.Credits-p.Price)
	e, _, err := s.post(ctx, ledger.Entry{Tenant: p.Tenant, Kind: ledger.KindPurchase, IdempotencyKey: "stripe:pi:" + intent.ID,
		Postings: lines.lines(), Reference: intent.ID, Actor: "stripe",
		Metadata: map[string]string{"purchase": p.ID, "pack": p.Pack, "user": p.User}})
	if err != nil {
		return err
	}
	_, err = s.updatePurchase(ctx, p.Tenant, p.ID, func(p *store.Purchase) bool {
		if p.EntryID == e.ID {
			return false
		}
		p.EntryID, p.StripeIntent = e.ID, intent.ID
		if p.Status == store.PurchasePending || p.Status == store.PurchaseFailed {
			p.Status = store.PurchaseSucceeded
		}
		return true
	})
	return err
}

func (s *Service) paymentFailed(ctx context.Context, intent stripe.PaymentIntent) error {
	p, ours, err := s.purchaseFor(ctx, intent.ID, intent.Metadata)
	if err != nil || !ours {
		return err
	}
	_, err = s.updatePurchase(ctx, p.Tenant, p.ID, func(p *store.Purchase) bool {
		if p.Status != store.PurchasePending {
			return false
		}
		p.Status = store.PurchaseFailed
		return true
	})
	return err
}

// creditsFor is the credits a refund of the cumulative amount `through`
// removes, beyond what refunds of `before` already removed. Proportional to
// the pack, so a full refund removes exactly the credits the purchase gave.
func creditsFor(p store.Purchase, before, through ledger.Amount) ledger.Amount {
	return through*p.Credits/p.Price - before*p.Credits/p.Price
}

// Refund refunds part (amount > 0) or the rest (amount 0) of a purchase.
// The ledger moves first; Stripe is then asked, and if it definitively
// refuses, the entry is reversed. A user who has spent the credits cannot
// be refunded through MOTOM: ErrInsufficientFunds.
func (s *Service) Refund(ctx context.Context, actor, tenant, id string, amount ledger.Amount, idempotencyKey string) (store.Purchase, ledger.Entry, error) {
	if s.stripe == nil {
		return store.Purchase{}, ledger.Entry{}, ErrPaymentsUnavailable
	}
	if err := validKey(idempotencyKey); err != nil {
		return store.Purchase{}, ledger.Entry{}, err
	}
	key := "refund:" + id + ":" + idempotencyKey
	e, found, err := s.replay(ctx, tenant, key)
	if err != nil {
		return store.Purchase{}, ledger.Entry{}, err
	}
	if !found {
		if e, err = s.postRefund(ctx, actor, tenant, id, amount, key); err != nil {
			return store.Purchase{}, ledger.Entry{}, err
		}
	} else if amount != 0 && e.Metadata["amount"] != strconv.FormatInt(int64(amount), 10) {
		return store.Purchase{}, ledger.Entry{}, store.ErrIdempotencyMismatch
	}
	if _, err := s.store.FindReversal(ctx, tenant, e.ID); err == nil {
		return store.Purchase{}, ledger.Entry{}, ErrStripeRefused
	}
	p, err := s.store.GetPurchase(ctx, tenant, id)
	if err != nil {
		return store.Purchase{}, ledger.Entry{}, err
	}
	refunded, _ := strconv.ParseInt(e.Metadata["amount"], 10, 64)
	_, err = s.stripe.CreateRefund(ctx, p.StripeIntent, refunded,
		map[string]string{"motom_tenant": tenant, "motom_purchase": id, "motom_entry": e.ID}, "motom:"+e.ID)
	if err != nil {
		if !stripe.Definitive(err) {
			// Unknown outcome: the entry stands and a retry with the same
			// key asks Stripe again under the same idempotency key.
			return store.Purchase{}, ledger.Entry{}, err
		}
		if uerr := s.unwind(ctx, e, "stripe refused the refund: "+err.Error()); uerr != nil {
			return store.Purchase{}, ledger.Entry{}, errors.Join(err, uerr)
		}
		if _, uerr := s.updatePurchase(ctx, tenant, id, func(p *store.Purchase) bool {
			p.Refunded -= ledger.Amount(refunded)
			if p.Status == store.PurchaseRefunded {
				p.Status = store.PurchaseSucceeded
			}
			return true
		}); uerr != nil {
			return store.Purchase{}, ledger.Entry{}, errors.Join(err, uerr)
		}
		return store.Purchase{}, ledger.Entry{}, fmt.Errorf("%w: %v", ErrStripeRefused, err)
	}
	p, err = s.store.GetPurchase(ctx, tenant, id)
	return p, e, err
}

// postRefund reserves the refund on the purchase, so two refunds cannot
// together exceed the price, then posts it.
func (s *Service) postRefund(ctx context.Context, actor, tenant, id string, amount ledger.Amount, key string) (ledger.Entry, error) {
	for attempt := 0; attempt < 8; attempt++ {
		p, err := s.store.GetPurchase(ctx, tenant, id)
		if err != nil {
			return ledger.Entry{}, err
		}
		if p.Status != store.PurchaseSucceeded || p.EntryID == "" {
			return ledger.Entry{}, fmt.Errorf("%w: only a paid, undisputed purchase can be refunded (this one is %s)", ErrInvalid, p.Status)
		}
		remaining := p.Price - p.Refunded
		if amount == 0 {
			amount = remaining
		}
		if amount <= 0 || amount > remaining {
			return ledger.Entry{}, fmt.Errorf("%w: %d of %d remains refundable", ErrInvalid, remaining, p.Price)
		}
		before := p.Refunded
		credits := creditsFor(p, before, before+amount)
		p.Refunded += amount
		if p.Refunded == p.Price {
			p.Status = store.PurchaseRefunded
		}
		p.UpdatedAt = s.now()
		if err := s.store.SavePurchase(ctx, p); errors.Is(err, store.ErrVersionConflict) {
			continue
		} else if err != nil {
			return ledger.Entry{}, err
		}
		release := func() {
			_, _ = s.updatePurchase(ctx, tenant, id, func(p *store.Purchase) bool {
				p.Refunded -= amount
				if p.Status == store.PurchaseRefunded {
					p.Status = store.PurchaseSucceeded
				}
				return true
			})
		}
		ids, err := s.accounts(ctx, tenant, p.Currency, accountSpec{ledger.Wallet, p.User},
			accountSpec{ledger.StripeClearing, ""}, accountSpec{ledger.Tenant, ""})
		if err != nil {
			release()
			return ledger.Entry{}, err
		}
		lines := (&postings{}).add(ids[0], credits).add(ids[1], -amount).add(ids[2], -(credits - amount))
		e, created, err := s.post(ctx, ledger.Entry{Tenant: tenant, Kind: ledger.KindRefund, IdempotencyKey: key,
			Postings: lines.lines(), Reference: p.StripeIntent, Actor: actor,
			Metadata: map[string]string{"purchase": id, "user": p.User, "amount": strconv.FormatInt(int64(amount), 10)}})
		if errors.Is(err, store.ErrIdempotencyMismatch) {
			// A concurrent identical request posted first, against a
			// different reservation. Give ours back and use theirs.
			release()
			return s.store.FindEntry(ctx, tenant, key)
		}
		if err != nil {
			release()
			return ledger.Entry{}, err
		}
		if !created {
			release()
		}
		return e, nil
	}
	return ledger.Entry{}, ErrConflict
}

// unwind reverses an entry whose outside half failed.
func (s *Service) unwind(ctx context.Context, e ledger.Entry, why string) error {
	_, _, err := s.post(ctx, ledger.Reverse(e, s.newID("je"), "unwind:"+e.ID, "motom", why, s.now()))
	if errors.Is(err, store.ErrAlreadyReversed) {
		return nil
	}
	return err
}

// liabilityAccount is who absorbs what a wallet cannot give back.
func (s *Service) liabilityAccount(ctx context.Context, t store.Tenant, currency ledger.Currency) (ledger.AccountID, error) {
	if t.Config.ChargebackLiability == store.LiabilityTenant {
		return s.account(ctx, t.ID, ledger.Tenant, "", currency)
	}
	return s.account(ctx, t.ID, ledger.Platform, "", currency)
}

// clawback posts money leaving through Stripe without MOTOM asking (a
// dashboard refund, a chargeback): the clearing account loses `amount`, the
// wallet gives back the matching credits as far as it can, and the shortfall
// lands on whoever bears the liability. Retried on version conflict, so the
// wallet balance it reads is the one it posts against.
func (s *Service) clawback(ctx context.Context, p store.Purchase, kind ledger.EntryKind, key, reference string, amount, credits ledger.Amount) (ledger.Entry, error) {
	if e, ok, err := s.replay(ctx, p.Tenant, key); ok || err != nil {
		return e, err
	}
	t, err := s.store.GetTenant(ctx, p.Tenant)
	if err != nil {
		return ledger.Entry{}, err
	}
	ids, err := s.accounts(ctx, p.Tenant, p.Currency, accountSpec{ledger.Wallet, p.User},
		accountSpec{ledger.StripeClearing, ""}, accountSpec{ledger.Tenant, ""})
	if err != nil {
		return ledger.Entry{}, err
	}
	liability, err := s.liabilityAccount(ctx, t, p.Currency)
	if err != nil {
		return ledger.Entry{}, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		available, version, err := s.normalBalance(ctx, ids[0])
		if err != nil {
			return ledger.Entry{}, err
		}
		fromWallet := min(max(available, 0), credits)
		lines := (&postings{}).add(ids[0], fromWallet).add(liability, credits-fromWallet).
			add(ids[1], -amount).add(ids[2], -(credits - amount))
		e, _, err := s.post(ctx, ledger.Entry{Tenant: p.Tenant, Kind: kind, IdempotencyKey: key,
			Postings: lines.lines(), Reference: reference, Actor: "stripe",
			Expect: map[ledger.AccountID]int64{ids[0]: version},
			Metadata: map[string]string{"purchase": p.ID, "user": p.User, "amount": strconv.FormatInt(int64(amount), 10),
				"shortfall": strconv.FormatInt(int64(credits-fromWallet), 10)}})
		if errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrInsufficientFunds) {
			continue
		}
		return e, err
	}
	return ledger.Entry{}, ErrConflict
}

func (s *Service) chargeRefunded(ctx context.Context, charge stripe.Charge) error {
	p, ours, err := s.purchaseFor(ctx, charge.PaymentIntent, nil)
	if err != nil || !ours || p.EntryID == "" {
		return err
	}
	total := ledger.Amount(charge.AmountRefunded)
	if total > p.Price {
		total = p.Price
	}
	for attempt := 0; attempt < 8; attempt++ {
		p, err = s.store.GetPurchase(ctx, p.Tenant, p.ID)
		if err != nil {
			return err
		}
		if total <= p.Refunded {
			// Every refund Stripe reports is one MOTOM already posted.
			return nil
		}
		before := p.Refunded
		p.Refunded, p.UpdatedAt = total, s.now()
		if p.Refunded == p.Price {
			p.Status = store.PurchaseRefunded
		}
		if err := s.store.SavePurchase(ctx, p); errors.Is(err, store.ErrVersionConflict) {
			continue
		} else if err != nil {
			return err
		}
		key := "stripe:refunded:" + charge.PaymentIntent + ":" + strconv.FormatInt(int64(total), 10)
		if _, err := s.clawback(ctx, p, ledger.KindRefund, key, charge.ID, total-before, creditsFor(p, before, total)); err != nil {
			return err
		}
		_, err = s.flag(ctx, p.Tenant, "purchase:"+p.ID, "refund_outside_motom",
			fmt.Sprintf("Stripe reports %d refunded; MOTOM had posted %d", total, before), charge.ID)
		return err
	}
	return ErrConflict
}

func (s *Service) disputeOpened(ctx context.Context, d stripe.Dispute) error {
	p, ours, err := s.purchaseFor(ctx, d.PaymentIntent, nil)
	if err != nil || !ours || p.EntryID == "" {
		return err
	}
	amount := min(ledger.Amount(d.Amount), p.Price)
	if _, err := s.clawback(ctx, p, ledger.KindChargeback, "stripe:dispute:"+d.ID, d.ID, amount, creditsFor(p, 0, amount)); err != nil {
		return err
	}
	if _, err := s.updatePurchase(ctx, p.Tenant, p.ID, func(p *store.Purchase) bool {
		if p.Status == store.PurchaseDisputed {
			return false
		}
		p.Status = store.PurchaseDisputed
		return true
	}); err != nil {
		return err
	}
	if _, err := s.SetFrozen(ctx, "stripe", p.Tenant, p.User, true, "chargeback "+d.ID); err != nil {
		return err
	}
	_, err = s.flag(ctx, p.Tenant, "wallet:"+p.User, "chargeback", fmt.Sprintf("dispute %s (%s) on purchase %s", d.ID, d.Reason, p.ID), d.ID)
	return err
}

func (s *Service) disputeClosed(ctx context.Context, d stripe.Dispute) error {
	p, ours, err := s.purchaseFor(ctx, d.PaymentIntent, nil)
	if err != nil || !ours {
		return err
	}
	chargeback, err := s.store.FindEntry(ctx, p.Tenant, "stripe:dispute:"+d.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s.audit(ctx, p.Tenant, "stripe", "dispute."+d.Status, "purchase:"+p.ID, d.ID)
	if d.Status != "won" && d.Status != "warning_closed" {
		return nil
	}
	// Won: Stripe returns the money, so the chargeback is undone. The wallet
	// stays frozen until a reviewer resolves the chargeback review.
	_, _, err = s.post(ctx, ledger.Reverse(chargeback, s.newID("je"), "stripe:dispute:"+d.ID+":won", "stripe", "dispute won", s.now()))
	if err != nil && !errors.Is(err, store.ErrAlreadyReversed) {
		return err
	}
	_, err = s.updatePurchase(ctx, p.Tenant, p.ID, func(p *store.Purchase) bool {
		if p.Status != store.PurchaseDisputed {
			return false
		}
		p.Status = store.PurchaseSucceeded
		return true
	})
	return err
}
