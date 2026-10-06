// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/moqom-cloud/motom/events"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// TipRequest is a viewer tipping a creator in a MOQOM room.
type TipRequest struct {
	Room           string
	Sender         string
	Creator        string
	Amount         ledger.Amount
	Currency       ledger.Currency
	Message        string
	IdempotencyKey string
}

// TipResult is a tip that has moved money.
type TipResult struct {
	Tip   store.Tip
	Entry ledger.Entry
	Event events.Tip
	// Replayed is a request whose idempotency key had already tipped; no
	// money moved and no event was broadcast this time.
	Replayed bool
}

func cleanMessage(message string) (string, error) {
	message = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, message))
	if len(message) > events.MaxMessage {
		return "", fmt.Errorf("%w: a tip message is at most %d bytes", ErrInvalid, events.MaxMessage)
	}
	return message, nil
}

func tipEvent(t store.Tip) events.Tip {
	return events.Tip{Type: events.TipType, ID: t.ID, Tenant: t.Tenant, Room: t.Room, From: t.Sender, To: t.Creator,
		Amount: int64(t.Amount), Currency: string(t.Currency), Message: t.Message, CreatedAt: t.CreatedAt}
}

// Tip moves credit from a viewer's wallet to a creator's earnings, less the
// platform and tenant fees, and broadcasts the tip to the room.
func (s *Service) Tip(ctx context.Context, actor, tenant string, r TipRequest) (TipResult, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return TipResult{}, err
	}
	for field, value := range map[string]string{"room": r.Room, "sender": r.Sender, "creator": r.Creator} {
		if err := validID(field, value); err != nil {
			return TipResult{}, err
		}
	}
	message, err := cleanMessage(r.Message)
	if err != nil {
		return TipResult{}, err
	}
	key := "tip:" + r.IdempotencyKey
	id := derivedID("tip", tenant, r.IdempotencyKey)
	if e, ok, err := s.replay(ctx, tenant, key); err != nil {
		return TipResult{}, err
	} else if ok {
		if err := sameRequest(e, r.Sender, r.Amount, r.Currency, map[string]string{"room": r.Room, "creator": r.Creator}); err != nil {
			return TipResult{}, err
		}
		return s.tipReplayed(ctx, tenant, id, e)
	}
	if r.Sender == r.Creator {
		s.audit(ctx, tenant, actor, "fraud.self_tip", "wallet:"+r.Sender, r.Room)
		return TipResult{}, fmt.Errorf("%w: a user cannot tip themselves", ErrInvalid)
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return TipResult{}, err
	}
	currency, err := s.currency(t, r.Currency)
	if err != nil {
		return TipResult{}, err
	}
	c := t.Config
	if r.Amount < c.TipMin || r.Amount > c.TipMax {
		return TipResult{}, fmt.Errorf("%w: a tip is %d-%d", ErrInvalid, c.TipMin, c.TipMax)
	}
	w, err := s.ensureWallet(ctx, tenant, r.Sender)
	if err != nil {
		return TipResult{}, err
	}
	if w.Frozen {
		return TipResult{}, ErrFrozen
	}
	if payee, err := s.store.GetPayee(ctx, tenant, r.Creator); err == nil && payee.Frozen {
		return TipResult{}, fmt.Errorf("%w: the creator's account", ErrFrozen)
	}

	lock := s.senderLock(tenant, r.Sender)
	lock.Lock()
	defer lock.Unlock()

	now := s.now()
	if err := s.tipLimits(ctx, t, r, now); err != nil {
		s.audit(ctx, tenant, actor, "fraud.tip_limit", "wallet:"+r.Sender, err.Error())
		return TipResult{}, err
	}
	ids, err := s.accounts(ctx, tenant, currency, accountSpec{ledger.Wallet, r.Sender},
		accountSpec{ledger.Earnings, r.Creator}, accountSpec{ledger.Fees, ""}, accountSpec{ledger.Tenant, ""})
	if err != nil {
		return TipResult{}, err
	}
	fees, toCreator := ledger.Split(r.Amount, t.PlatformFee, c.TenantFee)
	lines := (&postings{}).add(ids[0], r.Amount).add(ids[1], -toCreator).add(ids[2], -fees[0]).add(ids[3], -fees[1])
	var e ledger.Entry
	var before ledger.Amount
	for attempt := 0; ; attempt++ {
		var version int64
		before, version, err = s.normalBalance(ctx, ids[0])
		if err != nil {
			return TipResult{}, err
		}
		if before < r.Amount {
			return TipResult{}, ErrInsufficientFunds
		}
		var created bool
		e, created, err = s.post(ctx, ledger.Entry{Tenant: tenant, Kind: ledger.KindTip, IdempotencyKey: key,
			Postings: lines.lines(), Reference: id, Actor: actor, CreatedAt: now,
			Expect: map[ledger.AccountID]int64{ids[0]: version},
			Metadata: map[string]string{"tip": id, "room": r.Room, "sender": r.Sender, "creator": r.Creator,
				"platform_fee": strconv.FormatInt(int64(fees[0]), 10), "tenant_fee": strconv.FormatInt(int64(fees[1]), 10)}})
		if errors.Is(err, store.ErrVersionConflict) && attempt < 8 {
			// The wallet moved between the read and the post (a spend, a
			// tip from another replica): re-read and try again.
			continue
		}
		if err != nil {
			return TipResult{}, err
		}
		if !created {
			return s.tipReplayed(ctx, tenant, id, e)
		}
		break
	}
	tip := store.Tip{ID: id, Tenant: tenant, Room: r.Room, Sender: r.Sender, Creator: r.Creator, Currency: currency,
		Amount: r.Amount, Message: message, EntryID: e.ID, CreatedAt: now}
	if err := s.store.CreateTip(ctx, tip); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		// The money has moved; the tip row is a projection of the entry and
		// a replay of the request rebuilds it.
		s.log.ErrorContext(ctx, "tip record failed", "event", "motom_tip_record_failed", "tip", id, "error", err)
	}
	s.tipFlags(ctx, t, r, w, before, now)
	event := tipEvent(tip)
	if err := s.broadcaster.BroadcastTip(ctx, event); err != nil {
		s.log.WarnContext(ctx, "tip broadcast failed", "event", "motom_tip_broadcast_failed", "tip", id, "room", r.Room, "error", err)
	}
	return TipResult{Tip: tip, Entry: e, Event: event}, nil
}

func (s *Service) tipLimits(ctx context.Context, t store.Tenant, r TipRequest, now time.Time) error {
	c := t.Config
	n, _, err := s.store.TipStats(ctx, store.TipQuery{Tenant: t.ID, Sender: r.Sender, Since: now.Add(-time.Minute)})
	if err != nil {
		return err
	}
	if n >= c.SenderTipsPerMin {
		return fmt.Errorf("%w: at most %d tips a minute from one viewer", ErrRateLimited, c.SenderTipsPerMin)
	}
	n, _, err = s.store.TipStats(ctx, store.TipQuery{Tenant: t.ID, Room: r.Room, Since: now.Add(-time.Minute)})
	if err != nil {
		return err
	}
	if n >= c.RoomTipsPerMin {
		return fmt.Errorf("%w: at most %d tips a minute in one room", ErrRateLimited, c.RoomTipsPerMin)
	}
	day := now.Add(-24 * time.Hour)
	_, sum, err := s.store.TipStats(ctx, store.TipQuery{Tenant: t.ID, Sender: r.Sender, Since: day})
	if err != nil {
		return err
	}
	if sum+r.Amount > c.SenderDailyCap {
		return fmt.Errorf("%w: a viewer may tip at most %d in 24 hours", ErrLimitExceeded, c.SenderDailyCap)
	}
	_, sum, err = s.store.TipStats(ctx, store.TipQuery{Tenant: t.ID, Room: r.Room, Since: day})
	if err != nil {
		return err
	}
	if sum+r.Amount > c.RoomDailyCap {
		return fmt.Errorf("%w: a room may receive at most %d in 24 hours", ErrLimitExceeded, c.RoomDailyCap)
	}
	return nil
}

// FreshWallet is how new a wallet must be for draining it into one creator
// to look like a stolen card being cashed out.
const FreshWallet = time.Hour

// tipFlags queues reviews for patterns that are allowed but suspicious. A
// review on a creator holds their payouts until it is resolved; the tip
// itself stands.
func (s *Service) tipFlags(ctx context.Context, t store.Tenant, r TipRequest, w store.Wallet, before ledger.Amount, now time.Time) {
	subject := "payee:" + r.Creator
	var flags [][2]string
	if r.Amount >= t.Config.ReviewTipAt {
		flags = append(flags, [2]string{"large_tip", fmt.Sprintf("%d from %s in %s", r.Amount, r.Sender, r.Room)})
	}
	if n, _, err := s.store.TipStats(ctx, store.TipQuery{Tenant: t.ID, Sender: r.Sender, Creator: r.Creator, Since: now.Add(-time.Hour)}); err == nil && n > t.Config.RepeatPairPerHour {
		flags = append(flags, [2]string{"repeat_pair", fmt.Sprintf("%d tips from %s in an hour", n, r.Sender)})
	}
	if now.Sub(w.CreatedAt) < FreshWallet && r.Amount*5 >= before*4 {
		flags = append(flags, [2]string{"fresh_wallet_drain", fmt.Sprintf("%s tipped %d of %d from a wallet created %s ago", r.Sender, r.Amount, before, now.Sub(w.CreatedAt).Round(time.Second))})
	}
	for _, f := range flags {
		if _, err := s.flag(ctx, t.ID, subject, f[0], f[1], r.Room); err != nil {
			s.log.ErrorContext(ctx, "review flag failed", "event", "motom_flag_failed", "subject", subject, "error", err)
		}
	}
}

// tipReplayed answers a repeated tip request from what the first one did,
// rebuilding the tip record from the entry if writing it had failed.
func (s *Service) tipReplayed(ctx context.Context, tenant, id string, e ledger.Entry) (TipResult, error) {
	tips, err := s.store.ListTips(ctx, store.TipQuery{Tenant: tenant, Room: e.Metadata["room"], Sender: e.Metadata["sender"]})
	if err != nil {
		return TipResult{}, err
	}
	for _, tip := range tips {
		if tip.ID == id {
			return TipResult{Tip: tip, Entry: e, Event: tipEvent(tip), Replayed: true}, nil
		}
	}
	tip := store.Tip{ID: id, Tenant: tenant, Room: e.Metadata["room"], Sender: e.Metadata["sender"], Creator: e.Metadata["creator"],
		EntryID: e.ID, CreatedAt: e.CreatedAt}
	for _, p := range e.Postings {
		if _, kind, _, currency, _ := p.Account.Parse(); kind == ledger.Wallet {
			tip.Amount, tip.Currency = p.Amount, currency
		}
	}
	if err := s.store.CreateTip(ctx, tip); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return TipResult{}, err
	}
	return TipResult{Tip: tip, Entry: e, Event: tipEvent(tip), Replayed: true}, nil
}

// Tips lists tips in a room, newest first.
func (s *Service) Tips(ctx context.Context, tenant string, q store.TipQuery) ([]store.Tip, error) {
	q.Tenant = tenant
	return s.store.ListTips(ctx, q)
}
