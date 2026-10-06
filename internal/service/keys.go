// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/moqom-cloud/motom/internal/store"
)

// keyPrefix makes a leaked MOTOM key recognisable to secret scanners, and
// distinct from a MOQOM key (mqk_), so neither is accepted by the other.
const keyPrefix = "mtk_"

// ErrInvalidKey is every way a presented key can fail, as one error.
var ErrInvalidKey = errors.New("invalid api key")

// ErrForbidden is a valid key without the scope a route needs.
var ErrForbidden = errors.New("this key does not have the scope required")

// unknownKeyHash is compared against when a key ID is not found, so a miss
// costs the same as a wrong secret.
var unknownKeyHash = hashSecret("motom-unknown-key")

func hashSecret(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ParseKey splits a presented key into its ID and secret.
func ParseKey(presented string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(presented, keyPrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || len(id) != 16 || len(secret) != 43 {
		return "", "", false
	}
	return id, secret, true
}

// IssueKey creates a scoped key. The plaintext is returned once and never
// stored: only a SHA-256 of its 256-bit secret is.
func (s *Service) IssueKey(ctx context.Context, actor, tenant, name string, scopes []store.Scope) (store.Key, string, error) {
	if _, err := s.store.GetTenant(ctx, tenant); err != nil {
		return store.Key{}, "", err
	}
	if len(name) > 100 {
		return store.Key{}, "", fmt.Errorf("%w: name longer than 100 bytes", ErrInvalid)
	}
	if len(scopes) == 0 {
		return store.Key{}, "", fmt.Errorf("%w: a key needs at least one scope", ErrInvalid)
	}
	unique := map[store.Scope]bool{}
	var clean []store.Scope
	for _, scope := range scopes {
		known := false
		for _, s := range store.Scopes {
			known = known || s == scope
		}
		if !known {
			return store.Key{}, "", fmt.Errorf("%w: unknown scope %q", ErrInvalid, scope)
		}
		if !unique[scope] {
			unique[scope] = true
			clean = append(clean, scope)
		}
	}
	var id [8]byte
	var secret [32]byte
	if _, err := io.ReadFull(s.random, id[:]); err != nil {
		return store.Key{}, "", err
	}
	if _, err := io.ReadFull(s.random, secret[:]); err != nil {
		return store.Key{}, "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret[:])
	key := store.Key{ID: hex.EncodeToString(id[:]), Tenant: tenant, Name: name, Scopes: clean,
		Hash: hashSecret(encoded), CreatedAt: s.now()}
	if err := s.store.PutKey(ctx, key); err != nil {
		return store.Key{}, "", err
	}
	s.audit(ctx, tenant, actor, "key.issue", "key:"+key.ID, fmt.Sprintf(`{"scopes":%q}`, clean))
	return key, keyPrefix + key.ID + "_" + encoded, nil
}

// ResolveKey checks a presented key in constant time and returns it.
func (s *Service) ResolveKey(ctx context.Context, presented string) (store.Key, error) {
	id, secret, ok := ParseKey(presented)
	if !ok {
		return store.Key{}, ErrInvalidKey
	}
	key, err := s.store.GetKey(ctx, id)
	stored := key.Hash
	if err != nil {
		stored = unknownKeyHash
	}
	match := subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(stored)) == 1
	if err != nil || !match || key.Revoked() {
		return store.Key{}, ErrInvalidKey
	}
	return key, nil
}

// ListKeys lists a tenant's keys, without secrets.
func (s *Service) ListKeys(ctx context.Context, tenant string) ([]store.Key, error) {
	return s.store.ListKeys(ctx, tenant)
}

// RevokeKey revokes a key.
func (s *Service) RevokeKey(ctx context.Context, actor, tenant, id string) error {
	if err := s.store.RevokeKey(ctx, tenant, id, s.now()); err != nil {
		return err
	}
	s.audit(ctx, tenant, actor, "key.revoke", "key:"+id, "")
	return nil
}
