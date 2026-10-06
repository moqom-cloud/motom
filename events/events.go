// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package events is what MOTOM tells a MOQOM room: the tip event schema and
// the hook interface a room integration implements to broadcast it.
//
// MOTOM does not talk to the relay. It calls a Broadcaster after a tip's
// ledger entry has committed; whoever implements Broadcaster (the tenant's
// backend, or a MOQOM control-plane adapter) decides how the event reaches the
// room, for example as a data-channel message to every participant.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TipType is the event type of a tip, versioned so a room client can ignore
// a schema it does not understand.
const TipType = "motom.tip.v1"

// Tip is broadcast to a room when a viewer tips a creator.
//
// It carries what a room needs to render the moment and nothing more: no
// balances, no fee split, no payment details. Amount is what the viewer
// gave, in minor units of Currency.
type Tip struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant"`
	Room      string    `json:"room"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks a tip event before it is sent or after it is received.
func (t Tip) Validate() error {
	switch {
	case t.Type != TipType:
		return fmt.Errorf("events: type %q, want %q", t.Type, TipType)
	case t.ID == "" || t.Tenant == "" || t.Room == "" || t.From == "" || t.To == "":
		return fmt.Errorf("events: id, tenant, room, from and to are required")
	case t.Amount <= 0 || len(t.Currency) != 3:
		return fmt.Errorf("events: a positive amount and a three-letter currency are required")
	case len(t.Message) > MaxMessage:
		return fmt.Errorf("events: message longer than %d bytes", MaxMessage)
	}
	return nil
}

// MaxMessage bounds a tip message in bytes.
const MaxMessage = 280

// Marshal encodes the event as the JSON a room receives.
func (t Tip) Marshal() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(t)
}

// Broadcaster delivers events to rooms. MOTOM calls it once per tip, after
// the money has moved; an error is logged and does not undo the tip, so an
// implementation that must not lose events should queue rather than fail.
type Broadcaster interface {
	BroadcastTip(ctx context.Context, tip Tip) error
}

// BroadcasterFunc adapts a function to Broadcaster.
type BroadcasterFunc func(ctx context.Context, tip Tip) error

func (f BroadcasterFunc) BroadcastTip(ctx context.Context, tip Tip) error { return f(ctx, tip) }

// Discard is a Broadcaster that drops every event.
var Discard Broadcaster = BroadcasterFunc(func(context.Context, Tip) error { return nil })
