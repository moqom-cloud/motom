// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

// DefaultConfig is where a tenant starts. Every number here is a placeholder
// for Elliott to decide (see README, "Open decisions"); amounts are minor
// units of the tenant's currency.
func DefaultConfig() store.TenantConfig {
	return store.TenantConfig{
		TenantFee:           0,
		TipMin:              50,
		TipMax:              50_000,
		SenderDailyCap:      100_000,
		RoomDailyCap:        2_000_000,
		SenderTipsPerMin:    10,
		RoomTipsPerMin:      300,
		PurchasesPerHour:    10,
		ReviewTipAt:         25_000,
		RepeatPairPerHour:   30,
		HoldDays:            7,
		MinPayout:           1_000,
		ReviewFirstPayoutAt: 50_000,
		ChargebackLiability: store.LiabilityPlatform,
	}
}

// MaxPackPrice bounds a single pack.
const MaxPackPrice ledger.Amount = 1_000_000

// MaxHoldDays bounds a payout hold.
const MaxHoldDays = 90

func validateConfig(c store.TenantConfig, currencies []ledger.Currency, platformFee ledger.BasisPoints, minHoldDays int) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	if c.TenantFee < 0 || platformFee < 0 || c.TenantFee+platformFee >= 10_000 {
		return bad("tenant fee %d bps plus platform fee %d bps must leave the creator something", c.TenantFee, platformFee)
	}
	seen := map[string]bool{}
	for _, p := range c.Packs {
		if err := validID("pack id", p.ID); err != nil {
			return err
		}
		if seen[p.ID] {
			return bad("pack %s appears twice", p.ID)
		}
		seen[p.ID] = true
		if p.Price <= 0 || p.Price > MaxPackPrice || p.Bonus < 0 || p.Bonus > p.Price {
			return bad("pack %s: price must be 1-%d and bonus 0-price", p.ID, MaxPackPrice)
		}
		if currencies != nil && !contains(currencies, p.Currency) {
			return bad("pack %s: currency %s is not one of the tenant's", p.ID, p.Currency)
		}
		if len(p.Name) > 100 {
			return bad("pack %s: name longer than 100 bytes", p.ID)
		}
	}
	if c.TipMin <= 0 || c.TipMax < c.TipMin || c.TipMax > ledger.MaxAmount {
		return bad("tip bounds must satisfy 0 < min <= max")
	}
	if c.SenderDailyCap < c.TipMin || c.RoomDailyCap < c.TipMin {
		return bad("daily caps must allow at least one minimum tip")
	}
	if c.SenderTipsPerMin <= 0 || c.RoomTipsPerMin <= 0 || c.PurchasesPerHour <= 0 || c.RepeatPairPerHour <= 0 {
		return bad("rate limits must be positive")
	}
	if c.ReviewTipAt <= 0 || c.ReviewFirstPayoutAt <= 0 {
		return bad("review thresholds must be positive")
	}
	if c.HoldDays < minHoldDays || c.HoldDays > MaxHoldDays {
		return bad("hold days must be %d-%d", minHoldDays, MaxHoldDays)
	}
	if c.MinPayout <= 0 {
		return bad("minimum payout must be positive")
	}
	if c.ChargebackLiability != store.LiabilityPlatform && c.ChargebackLiability != store.LiabilityTenant {
		return bad("chargeback liability is platform or tenant")
	}
	return nil
}

func contains(currencies []ledger.Currency, c ledger.Currency) bool {
	for _, known := range currencies {
		if known == c {
			return true
		}
	}
	return false
}

// CreateTenant onboards a MOQOM tenant to MOTOM. Operator only: the
// platform fee is MOTOM's commercial term, not the tenant's to set.
func (s *Service) CreateTenant(ctx context.Context, actor, id string, currencies []ledger.Currency, platformFee ledger.BasisPoints) (store.Tenant, error) {
	if err := validID("tenant", id); err != nil {
		return store.Tenant{}, err
	}
	if len(currencies) == 0 {
		return store.Tenant{}, fmt.Errorf("%w: at least one currency", ErrInvalid)
	}
	for _, c := range currencies {
		if !c.Valid() {
			return store.Tenant{}, fmt.Errorf("%w: currency %q", ErrInvalid, c)
		}
	}
	config := s.defaults
	config.Packs = nil
	if err := validateConfig(config, currencies, platformFee, s.minHoldDays); err != nil {
		return store.Tenant{}, err
	}
	now := s.now()
	t := store.Tenant{ID: id, Currencies: currencies, PlatformFee: platformFee, Config: config, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateTenant(ctx, t); err != nil {
		return store.Tenant{}, err
	}
	s.audit(ctx, id, actor, "tenant.create", "tenant:"+id, fmt.Sprintf(`{"platform_fee_bps":%d}`, platformFee))
	return s.store.GetTenant(ctx, id)
}

// Tenant returns a tenant.
func (s *Service) Tenant(ctx context.Context, id string) (store.Tenant, error) {
	return s.store.GetTenant(ctx, id)
}

// UpdateConfig replaces a tenant's own configuration.
func (s *Service) UpdateConfig(ctx context.Context, actor, tenant string, config store.TenantConfig) (store.Tenant, error) {
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Tenant{}, err
	}
	if err := validateConfig(config, t.Currencies, t.PlatformFee, s.minHoldDays); err != nil {
		return store.Tenant{}, err
	}
	t.Config, t.UpdatedAt = config, s.now()
	if err := s.store.SaveTenant(ctx, t); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return store.Tenant{}, ErrConflict
		}
		return store.Tenant{}, err
	}
	detail, _ := json.Marshal(config)
	s.audit(ctx, tenant, actor, "tenant.config", "tenant:"+tenant, string(detail))
	return s.store.GetTenant(ctx, tenant)
}

// SetPlatformFee changes MOTOM's fee for a tenant. Operator only.
func (s *Service) SetPlatformFee(ctx context.Context, actor, tenant string, bps ledger.BasisPoints) (store.Tenant, error) {
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Tenant{}, err
	}
	if err := validateConfig(t.Config, t.Currencies, bps, s.minHoldDays); err != nil {
		return store.Tenant{}, err
	}
	t.PlatformFee, t.UpdatedAt = bps, s.now()
	if err := s.store.SaveTenant(ctx, t); err != nil {
		return store.Tenant{}, err
	}
	s.audit(ctx, tenant, actor, "tenant.platform_fee", "tenant:"+tenant, fmt.Sprintf(`{"platform_fee_bps":%d}`, bps))
	return s.store.GetTenant(ctx, tenant)
}

func (s *Service) currency(t store.Tenant, requested ledger.Currency) (ledger.Currency, error) {
	if requested == "" {
		return t.Currencies[0], nil
	}
	if !contains(t.Currencies, requested) {
		return "", fmt.Errorf("%w: currency %s is not enabled for this tenant", ErrInvalid, requested)
	}
	return requested, nil
}

func pack(t store.Tenant, id string) (store.Pack, bool) {
	for _, p := range t.Config.Packs {
		if p.ID == id {
			return p, true
		}
	}
	return store.Pack{}, false
}

// TrialBalance sums every posting of a tenant by currency; each sum is zero.
func (s *Service) TrialBalance(ctx context.Context, tenant string) (map[ledger.Currency]ledger.Amount, error) {
	return s.store.TrialBalance(ctx, tenant)
}

// Audit lists audit records for a tenant, optionally for one subject.
func (s *Service) Audit(ctx context.Context, tenant, subject string, limit int) ([]store.AuditRecord, error) {
	return s.store.ListAudit(ctx, tenant, subject, limit)
}
