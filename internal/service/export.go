// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// For a tenant's end users MOTOM is a processor: the tenant is the controller
// and answers its users' data subject requests, and these two calls are how
// MOTOM assists (docs/COMPLIANCE.md). Users are identified only by the opaque
// ID the tenant assigned.

// UserExport is everything MOTOM holds about one of a tenant's users.
type UserExport struct {
	GeneratedAt  time.Time
	Tenant       string
	User         string
	Wallet       WalletState
	Entries      []ledger.Entry
	Purchases    []store.Purchase
	TipsSent     []store.Tip
	TipsReceived []store.Tip
	Payee        *store.Payee
	Payouts      []store.Payout
	Reviews      []store.Review
}

// ExportUser assembles a user's export (GDPR Arts. 15 and 20, CCPA right to
// know). Reviews are included because they are decisions about the user.
func (s *Service) ExportUser(ctx context.Context, tenant, user string) (UserExport, error) {
	wallet, err := s.Wallet(ctx, tenant, user)
	if err != nil {
		return UserExport{}, err
	}
	out := UserExport{GeneratedAt: s.now(), Tenant: tenant, User: user, Wallet: wallet}
	if out.Entries, err = s.store.ListEntries(ctx, store.EntryFilter{Tenant: tenant, Owner: user}); err != nil {
		return UserExport{}, err
	}
	if out.Purchases, err = s.store.ListPurchases(ctx, tenant, user); err != nil {
		return UserExport{}, err
	}
	if out.TipsSent, err = s.store.ListTips(ctx, store.TipQuery{Tenant: tenant, Sender: user}); err != nil {
		return UserExport{}, err
	}
	if out.TipsReceived, err = s.store.ListTips(ctx, store.TipQuery{Tenant: tenant, Creator: user}); err != nil {
		return UserExport{}, err
	}
	payee, err := s.store.GetPayee(ctx, tenant, user)
	switch {
	case err == nil:
		out.Payee = &payee
		if out.Payouts, err = s.store.ListPayouts(ctx, store.PayoutQuery{Tenant: tenant, Creator: user}); err != nil {
			return UserExport{}, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return UserExport{}, err
	}
	for _, subject := range []string{"wallet:" + user, "payee:" + user} {
		reviews, err := s.store.ListReviews(ctx, tenant, "", subject)
		if err != nil {
			return UserExport{}, err
		}
		out.Reviews = append(out.Reviews, reviews...)
	}
	return out, nil
}

// Erasure is what an erasure request did and did not remove.
type Erasure struct {
	TipMessagesErased int
	Retained          []string
}

// EraseUser removes what MOTOM can lawfully remove: the free text of the
// user's tip messages. Ledger entries, purchases, tips' amounts, payouts and
// the audit log are financial records retained for seven years (GDPR Art.
// 17(3)(b), legal obligation) and keyed only by the tenant's opaque user ID;
// the tenant erases the mapping from that ID to a person on its side.
func (s *Service) EraseUser(ctx context.Context, actor, tenant, user string) (Erasure, error) {
	if err := validID("user", user); err != nil {
		return Erasure{}, err
	}
	n, err := s.store.ScrubTips(ctx, tenant, user)
	if err != nil {
		return Erasure{}, err
	}
	s.audit(ctx, tenant, actor, "dsar.erase", "wallet:"+user, "")
	return Erasure{TipMessagesErased: n, Retained: []string{
		"ledger entries and postings (financial record, 7 years)",
		"purchases, tip amounts and payouts (financial record, 7 years)",
		"audit log (financial record, 7 years)",
		"Stripe holds card, payment and Connect identity data under its own policies",
	}}, nil
}
