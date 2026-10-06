// Copyright 2026 Elliott Wave LLC. All rights reserved.

package store

import (
	"encoding/json"

	"github.com/moqom-cloud/motom/ledger"
)

// EntryAudit is the audit record a store writes in the same transaction as
// an entry, so a money movement without an audit line cannot exist.
func EntryAudit(e ledger.Entry) AuditRecord {
	type line struct {
		Account ledger.AccountID `json:"account"`
		Amount  ledger.Amount    `json:"amount"`
	}
	lines := make([]line, 0, len(e.Postings))
	for _, p := range e.Postings {
		lines = append(lines, line{p.Account, p.Amount})
	}
	detail, _ := json.Marshal(struct {
		Kind      ledger.EntryKind `json:"kind"`
		Key       string           `json:"idempotency_key"`
		Reverses  string           `json:"reverses,omitempty"`
		Reference string           `json:"reference,omitempty"`
		Postings  []line           `json:"postings"`
	}{e.Kind, e.IdempotencyKey, e.Reverses, e.Reference, lines})
	return AuditRecord{
		ID: "au_" + e.ID, Tenant: e.Tenant, Actor: e.Actor, Action: "ledger." + string(e.Kind),
		Subject: "entry:" + e.ID, Detail: string(detail), At: e.CreatedAt,
	}
}

// CheckOverdraw reports ErrInsufficientFunds if posting delta to an account
// holding signed would take an account that may not overdraw below zero.
func CheckOverdraw(account ledger.Account, signed, delta ledger.Amount) error {
	if account.Kind.MayOverdraw() || delta == 0 {
		return nil
	}
	after := account.Normal(signed + delta)
	if after < 0 && account.Normal(delta) < 0 {
		return ErrInsufficientFunds
	}
	return nil
}
