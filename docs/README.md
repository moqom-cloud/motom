# MOTOM

MOTOM is the money layer for MOQOM. It lets a MOQOM tenant sell credits to
its viewers, lets viewers tip creators during a live room, and pays creators
their earnings to a bank account.

## What it does for a tenant

- **Sell credits.** Viewers buy credit packs the tenant defines (price, and an
  optional bonus the tenant funds) through Stripe Checkout or Stripe Elements.
  Credits arrive when Stripe confirms the payment.
- **Tips in the room.** A viewer tips a creator; MOTOM moves the credits,
  takes the platform and tenant fees, and hands the tenant's room integration
  an event to show everyone in the room.
- **Pay creators.** Creators verify their identity and add a bank account on
  a Stripe-hosted page. After a configurable hold, scheduled payout runs send
  their earnings to them.
- **Handle the hard cases.** Refunds, chargebacks, failed payouts and returned
  transfers are each recorded as new ledger entries that undo the original,
  never as edits. A chargeback freezes the buyer's wallet and opens a review.
- **Catch abuse early.** Per-viewer and per-room rate limits and daily caps,
  and flags for suspicious patterns that hold a creator's payout until a
  person reviews them.
- **Keep the books.** A double-entry ledger in which every entry balances,
  every balance is computed from entries, and every money movement has an
  audit record. Financial records are kept for seven years.

## How it is built

A single Go service (`cmd/motom`) over Postgres, calling Stripe. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the ledger and money flows,
[API.md](API.md) for the HTTP API, [SECURITY.md](SECURITY.md) and
[COMPLIANCE.md](COMPLIANCE.md) for controls and open legal questions, and
[MCP-TOOLS.md](MCP-TOOLS.md) for proposed MOQOM MCP tools.

| Package | Role |
| --- | --- |
| `ledger` | Accounts, entries, postings, fees. Pure; no I/O. |
| `events` | The tip event a room receives and the `Broadcaster` hook. |
| `client` | Go client for the HTTP API. |
| `internal/service` | Credits, tips, payouts, fraud checks, webhooks, DSAR. |
| `internal/httpapi` | HTTP handlers, API-key auth, scopes, per-key rate limit. |
| `internal/store` | Store interface; `memory` and `postgres` implementations; `storetest` conformance suite. |
| `internal/stripe` | Minimal Stripe client and webhook verification; `stripefake` test server. |
| `deploy/terraform` | Draft Cloud Run, Cloud SQL, Secret Manager and Scheduler module. Not applied. |

## Quickstart (local, no money)

```sh
cd motom
go test ./...

# A throwaway server on the in-memory store, without Stripe.
MOTOM_DEV=1 go run ./cmd/motom serve
```

With `MOTOM_DEV=1` and no database configured the server keeps everything in
memory and loses it on exit. Purchases and payouts need Stripe and return
`503 payments_unavailable` without it; grants, spends and tips work.

Against Postgres (any database you are allowed to write to; MOTOM creates
`motom_*` tables):

```sh
export MOTOM_DATABASE_URL=postgres://...
go run ./cmd/motom migrate
go run ./cmd/motom tenant create -id acme -currencies usd -platform-fee-bps 500
go run ./cmd/motom key create -tenant acme -name bootstrap -scopes admin
```

`key create` prints the API key once on stdout. Store it in a secret manager.

```go
c := client.New("https://motom.example", os.Getenv("MOTOM_API_KEY"))
res, err := c.Tip(ctx, client.TipRequest{Room: "r1", Sender: "u1", Creator: "u2",
	Amount: 500, Currency: "usd"}, client.NewIdempotencyKey())
```

The Postgres conformance tests run when `MOTOM_TEST_DATABASE_URL` or
`DATABASE_URL` is set and skip otherwise. Each test creates its own schema.

## Licence

Proprietary. Copyright 2026 Elliott Wave LLC. All rights reserved. See
[../NOTICE](../NOTICE).

**Note for Elliott:** MOQOM's convention is BUSL-1.1 for server code
(`moqom/go`) and Apache-2.0 for SDKs. MOTOM was left proprietary as
instructed, because it is a new module that handles money and there is no
reason yet to grant source-available rights. If tenants are meant to embed
`client` or `events` in their own code, those two packages are the
candidates for Apache-2.0, matching the MOQOM SDKs. That is your call.
