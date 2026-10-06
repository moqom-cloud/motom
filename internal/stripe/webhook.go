// Copyright 2026 Elliott Wave LLC. All rights reserved.

package stripe

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is a webhook delivery.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	// Account is set on events from a connected account (Connect webhooks).
	Account  string `json:"account"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// ErrSignature is any delivery that cannot be shown to have come from
// Stripe, including one signed too long ago to be anything but a replay.
var ErrSignature = errors.New("stripe webhook signature does not verify")

// Tolerance is how old a signed delivery may be. Stripe's own default; it
// bounds how long a captured delivery can be replayed. Within it, replays are
// caught by the event-ID record.
const Tolerance = 5 * time.Minute

// VerifyWebhook checks the Stripe-Signature header against each secret (the
// platform endpoint's and the Connect endpoint's) and decodes the event.
func VerifyWebhook(payload []byte, header string, secrets []string, now time.Time) (Event, error) {
	var timestamp string
	var signatures [][]byte
	for _, part := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch name {
		case "t":
			timestamp = value
		case "v1":
			if decoded, err := hex.DecodeString(value); err == nil {
				signatures = append(signatures, decoded)
			}
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || len(signatures) == 0 {
		return Event{}, ErrSignature
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > Tolerance || age < -Tolerance {
		return Event{}, ErrSignature
	}
	verified := false
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		expected := sign(payload, timestamp, secret)
		for _, signature := range signatures {
			// Every comparison runs, so timing says nothing about which
			// secret or signature came closest.
			if hmac.Equal(signature, expected) {
				verified = true
			}
		}
	}
	if !verified {
		return Event{}, ErrSignature
	}
	var event Event
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decoding the event: %w", err)
	}
	if event.ID == "" || event.Type == "" {
		return Event{}, fmt.Errorf("event without an id or type")
	}
	return event, nil
}

func sign(payload []byte, timestamp, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return mac.Sum(nil)
}

// SignWebhook produces a Stripe-Signature header, for tests and the fake.
func SignWebhook(payload []byte, secret string, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(sign(payload, timestamp, secret))
}
