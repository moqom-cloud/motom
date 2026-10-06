// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moqom-cloud/motom/internal/store"
)

// The fraud basics, in one place:
//
//   - Velocity: tips per minute per sender and per room, purchases per hour
//     per user (refused with ErrRateLimited), and rolling 24-hour tip caps per
//     sender and per room (ErrLimitExceeded).
//   - Refused outright: tipping yourself; spending, tipping or buying from a
//     frozen wallet; tipping a frozen creator.
//   - Flagged for the review queue, money allowed to move: a tip at or above
//     the tenant's review threshold, one sender tipping one creator unusually
//     often, a new wallet tipping most of its balance at once, a creator's
//     first large payout, a refund made outside MOTOM, a reversed transfer, a
//     payment that does not match what was sold.
//   - Frozen until reviewed: a wallet with a chargeback against it.
//
// An open review on a creator holds their payouts. Resolving it is a person's
// decision, recorded in the audit log.

// flag opens a review unless one is already open for the same subject and
// reason, so a burst of suspicious tips makes one queue item.
func (s *Service) flag(ctx context.Context, tenant, subject, reason, detail, reference string) (store.Review, error) {
	open, err := s.store.ListReviews(ctx, tenant, store.ReviewOpen, subject)
	if err != nil {
		return store.Review{}, err
	}
	for _, r := range open {
		if r.Reason == reason {
			return r, nil
		}
	}
	now := s.now()
	r := store.Review{ID: s.newID("rv"), Tenant: tenant, Subject: subject, Reason: reason, Detail: detail,
		Reference: reference, Status: store.ReviewOpen, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateReview(ctx, r); err != nil {
		return store.Review{}, err
	}
	s.audit(ctx, tenant, "motom", "review.open", subject, reason)
	s.log.WarnContext(ctx, "review opened", "event", "motom_review_opened", "tenant", tenant, "subject", subject, "reason", reason)
	return r, nil
}

// approved reports whether a reviewer has approved subject for reason.
func (s *Service) approved(ctx context.Context, tenant, subject, reason string) (bool, error) {
	reviews, err := s.store.ListReviews(ctx, tenant, store.ReviewApproved, subject)
	if err != nil {
		return false, err
	}
	for _, r := range reviews {
		if r.Reason == reason {
			return true, nil
		}
	}
	return false, nil
}

// Reviews lists the review queue.
func (s *Service) Reviews(ctx context.Context, tenant string, status store.ReviewStatus) ([]store.Review, error) {
	return s.store.ListReviews(ctx, tenant, status, "")
}

// ResolveReview approves or rejects an open review. Approving a chargeback
// review unfreezes the wallet; rejecting a creator review freezes the
// creator's payouts. Everything else is recorded and left to the reviewer.
func (s *Service) ResolveReview(ctx context.Context, actor, tenant, id string, approve bool, note string) (store.Review, error) {
	if len(note) > 1000 {
		return store.Review{}, fmt.Errorf("%w: note longer than 1000 bytes", ErrInvalid)
	}
	r, err := s.store.GetReview(ctx, tenant, id)
	if err != nil {
		return store.Review{}, err
	}
	if r.Status != store.ReviewOpen {
		return store.Review{}, fmt.Errorf("%w: review is already %s", ErrInvalid, r.Status)
	}
	r.Status, r.ResolvedBy, r.Resolution, r.UpdatedAt = store.ReviewRejected, actor, note, s.now()
	if approve {
		r.Status = store.ReviewApproved
	}
	if err := s.store.SaveReview(ctx, r); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return store.Review{}, ErrConflict
		}
		return store.Review{}, err
	}
	s.audit(ctx, tenant, actor, "review."+string(r.Status), r.Subject, r.Reason+": "+note)
	kind, who, _ := strings.Cut(r.Subject, ":")
	switch {
	case kind == "wallet" && approve && r.Reason == "chargeback":
		if _, err := s.SetFrozen(ctx, actor, tenant, who, false, ""); err != nil {
			return r, err
		}
	case kind == "payee" && !approve:
		if _, err := s.SetPayeeFrozen(ctx, actor, tenant, who, true); err != nil {
			return r, err
		}
	}
	return s.store.GetReview(ctx, tenant, id)
}
