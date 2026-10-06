// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/moqom-cloud/motom/internal/stripe"
)

// ErrSignature is a webhook that does not verify.
var ErrSignature = stripe.ErrSignature

// HandleWebhook verifies and applies a Stripe delivery.
//
// Replay protection is two layers: the signature's timestamp must be within
// five minutes, and an event ID is applied once (a redelivery answers 200 and
// does nothing). Every handler is also idempotent through its ledger keys, so
// an event processed concurrently by two replicas still moves money once. A
// handler error forgets the event ID and answers 500, which Stripe retries.
func (s *Service) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	event, err := stripe.VerifyWebhook(payload, signature, s.webhookSecrets, s.clock())
	if err != nil {
		return err
	}
	fresh, err := s.store.RecordWebhook(ctx, event.ID, event.Type, s.now())
	if err != nil {
		return err
	}
	if !fresh {
		s.log.InfoContext(ctx, "stripe webhook replayed", "event", "motom_webhook_duplicate", "stripe_event", event.ID)
		return nil
	}
	if err := s.dispatch(ctx, event); err != nil {
		if ferr := s.store.ForgetWebhook(context.WithoutCancel(ctx), event.ID); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return fmt.Errorf("stripe event %s (%s): %w", event.ID, event.Type, err)
	}
	return nil
}

func decodeObject[T any](event stripe.Event) (T, error) {
	var object T
	if err := json.Unmarshal(event.Data.Object, &object); err != nil {
		return object, fmt.Errorf("%w: %s object: %v", ErrInvalid, event.Type, err)
	}
	return object, nil
}

func (s *Service) dispatch(ctx context.Context, event stripe.Event) error {
	switch event.Type {
	case "payment_intent.succeeded", "payment_intent.payment_failed":
		intent, err := decodeObject[stripe.PaymentIntent](event)
		if err != nil {
			return err
		}
		if event.Type == "payment_intent.succeeded" {
			return s.paymentSucceeded(ctx, intent)
		}
		return s.paymentFailed(ctx, intent)
	case "charge.refunded":
		charge, err := decodeObject[stripe.Charge](event)
		if err != nil {
			return err
		}
		return s.chargeRefunded(ctx, charge)
	case "charge.dispute.created":
		dispute, err := decodeObject[stripe.Dispute](event)
		if err != nil {
			return err
		}
		return s.disputeOpened(ctx, dispute)
	case "charge.dispute.closed":
		dispute, err := decodeObject[stripe.Dispute](event)
		if err != nil {
			return err
		}
		return s.disputeClosed(ctx, dispute)
	case "account.updated":
		account, err := decodeObject[stripe.Account](event)
		if err != nil {
			return err
		}
		if account.ID == "" {
			account.ID = event.Account
		}
		return s.accountUpdated(ctx, account)
	case "transfer.reversed":
		transfer, err := decodeObject[stripe.Transfer](event)
		if err != nil {
			return err
		}
		return s.transferReversed(ctx, transfer)
	}
	s.log.DebugContext(ctx, "stripe webhook ignored", "stripe_event", event.ID, "type", event.Type)
	return nil
}
