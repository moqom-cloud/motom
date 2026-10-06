// Copyright 2026 Elliott Wave LLC. All rights reserved.

package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTipSchema(t *testing.T) {
	tip := Tip{Type: TipType, ID: "tip_1", Tenant: "t_1", Room: "r_1", From: "u_1", To: "c_1",
		Amount: 500, Currency: "usd", Message: "gg", CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	body, err := tip.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"motom.tip.v1","id":"tip_1","tenant":"t_1","room":"r_1","from":"u_1","to":"c_1","amount":500,"currency":"usd","message":"gg","created_at":"2026-10-06T12:00:00Z"}`
	if string(body) != want {
		t.Fatalf("wire format changed:\n%s\n%s", body, want)
	}
	var back Tip
	if err := json.Unmarshal(body, &back); err != nil || back != tip {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	for name, mutate := range map[string]func(*Tip){
		"type":     func(t *Tip) { t.Type = "motom.tip.v0" },
		"room":     func(t *Tip) { t.Room = "" },
		"amount":   func(t *Tip) { t.Amount = 0 },
		"currency": func(t *Tip) { t.Currency = "dollars" },
		"message":  func(t *Tip) { t.Message = strings.Repeat("x", MaxMessage+1) },
	} {
		bad := tip
		mutate(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: validated", name)
		}
	}
}
