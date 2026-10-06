// Copyright 2026 Elliott Wave LLC. All rights reserved.

package stripe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVerifyWebhook(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{}}}`)
	secrets := []string{"whsec_platform", "whsec_connect"}

	for _, secret := range secrets {
		event, err := VerifyWebhook(payload, SignWebhook(payload, secret, now), secrets, now)
		if err != nil || event.ID != "evt_1" {
			t.Fatalf("%s: %v %+v", secret, err, event)
		}
	}
	cases := map[string]string{
		"wrong secret":    SignWebhook(payload, "whsec_other", now),
		"too old":         SignWebhook(payload, "whsec_platform", now.Add(-Tolerance-time.Second)),
		"from the future": SignWebhook(payload, "whsec_platform", now.Add(Tolerance+time.Second)),
		"empty":           "",
		"no signature":    "t=" + "1791288000",
		"garbage":         "t=abc,v1=zz",
	}
	for name, header := range cases {
		if _, err := VerifyWebhook(payload, header, secrets, now); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	tampered := []byte(strings.Replace(string(payload), "evt_1", "evt_2", 1))
	if _, err := VerifyWebhook(tampered, SignWebhook(payload, "whsec_platform", now), secrets, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered body verified: %v", err)
	}
	if _, err := VerifyWebhook(payload, SignWebhook(payload, "", now), []string{""}, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("empty secret verified: %v", err)
	}
	// A header carrying several v1 signatures (as during secret rotation)
	// verifies if any one does.
	rotated := SignWebhook(payload, "whsec_old", now) + ",v1=" + strings.Split(SignWebhook(payload, "whsec_platform", now), "v1=")[1]
	if _, err := VerifyWebhook(payload, rotated, secrets, now); err != nil {
		t.Fatalf("rotation: %v", err)
	}
}

func TestDefinitive(t *testing.T) {
	cases := map[int]bool{400: true, 402: true, 404: true, 409: false, 429: false, 500: false, 503: false}
	for status, want := range cases {
		if got := Definitive(&Error{Status: status}); got != want {
			t.Errorf("%d: %v", status, got)
		}
	}
	if Definitive(errors.New("timeout")) || Definitive(nil) {
		t.Fatal("a transport error is not definitive")
	}
	if (&Client{}).post(context.Background(), "/", nil, "", nil) != ErrNotConfigured {
		t.Fatal("unconfigured client")
	}
}
