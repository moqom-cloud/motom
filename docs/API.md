# MOTOM HTTP API

Base path `/v1/motom`. JSON in and out. Amounts are integers in the
currency's minor unit (USD cents). Currencies are lower-case ISO 4217 codes
and default to the tenant's first currency. The Go client in `client`
covers every route.

## Authentication

```
Authorization: Bearer mtk_<16 hex id>_<43 char secret>
```

Keys belong to one tenant and carry scopes. A key never sees another
tenant's data, because the tenant comes from the key and never from the
request. Each key is rate limited (default 600 requests a minute; 429 with
`Retry-After`).

| Scope | Grants |
| --- | --- |
| `admin` | everything, including keys, config, trial balance and audit |
| `wallets:read` | wallets, entries, packs, purchases |
| `wallets:write` | spend, grant |
| `purchases:write` | start purchases |
| `refunds:write` | refund purchases |
| `tips:read` / `tips:write` | list tips / send tips |
| `payouts:read` / `payouts:write` | creator status, payouts / onboarding, payout runs |
| `reviews:read` / `reviews:write` | review queue / resolve reviews, freeze and unfreeze wallets |
| `dsar` | user export and erasure |

## Idempotency

Every request that moves money needs `Idempotency-Key` (1–200 bytes). Retry
with the same key and the same body after any failure. MOTOM applies it at
most once and returns the original result. Tips return `"replayed": true`
on a replay. The same key with a different body is `422
idempotency_mismatch`.

## Errors

```json
{"error": {"code": "insufficient_funds", "message": "..."}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid` | Bad input, unknown JSON field, missing Idempotency-Key |
| 400 | `signature` | Stripe webhook signature or timestamp invalid |
| 401 | `unauthorized` | Missing, malformed, unknown or revoked key |
| 402 | `insufficient_funds` | Wallet balance too low |
| 403 | `forbidden` | Key lacks the scope |
| 403 | `frozen` | Wallet or creator frozen |
| 404 | `not_found` | |
| 409 | `conflict` / `already_exists` | Concurrent change; retry |
| 422 | `idempotency_mismatch` | Key reused with a different request |
| 429 | `rate_limited` / `daily_limit` | Velocity limit or daily cap |
| 502 | `stripe_refused` / `stripe_error` | Stripe refused or failed |
| 503 | `payments_unavailable` | Deployment has no Stripe key |
| 500 | `internal` | Details are logged, not returned |

## Routes

| Method | Path | Scope | Idempotency-Key |
| --- | --- | --- | --- |
| POST | `/webhooks/stripe` | Stripe signature | – |
| GET | `/tenant` | admin | |
| PUT | `/tenant/config` | admin | |
| GET | `/keys` | admin | |
| POST | `/keys` | admin | |
| DELETE | `/keys/{id}` | admin | |
| GET | `/packs` | wallets:read | |
| GET | `/wallets/{user}` | wallets:read | |
| GET | `/wallets/{user}/entries?limit=` | wallets:read | |
| POST | `/wallets/{user}/spend` | wallets:write | required |
| POST | `/wallets/{user}/grant` | wallets:write | required |
| POST | `/wallets/{user}/freeze` | reviews:write | |
| POST | `/wallets/{user}/unfreeze` | reviews:write | |
| POST | `/purchases` | purchases:write | required |
| GET | `/purchases/{id}` | wallets:read | |
| POST | `/purchases/{id}/refund` | refunds:write | required |
| POST | `/tips` | tips:write | required |
| GET | `/rooms/{room}/tips?limit=` | tips:read | |
| POST | `/creators/{creator}/onboarding` | payouts:write | |
| GET | `/creators/{creator}` | payouts:read | |
| GET | `/payouts?creator=&status=&limit=` | payouts:read | |
| POST | `/payouts/run` | payouts:write | (run name) |
| GET | `/reviews?status=open\|approved\|rejected\|all` | reviews:read | |
| POST | `/reviews/{id}/resolve` | reviews:write | |
| GET | `/users/{user}/export` | dsar | |
| POST | `/users/{user}/erase` | dsar | |
| GET | `/ledger/trial-balance` | admin | |
| GET | `/audit?subject=&limit=` | admin | |
| GET | `/healthz` (no prefix) | – | |

IDs (`user`, `creator`, `room`) are the tenant's own opaque identifiers:
1–128 printable bytes without `/` or spaces. Do not use email addresses or
names. MOTOM never needs to know who a user is.

### Buy credits

```http
POST /v1/motom/purchases
Idempotency-Key: 6f1c...
{"user": "u_42", "pack": "big", "mode": "checkout",
 "success_url": "https://app.example/ok", "cancel_url": "https://app.example/cancel"}
```

`201` with `status: "pending"` and `checkout_url` (redirect the user) or,
with `"mode": "payment_intent"`, `client_secret` (confirm with Stripe
Elements). Credits land when Stripe's webhook confirms payment, and the
status becomes `succeeded`.

### Tip

```http
POST /v1/motom/tips
Idempotency-Key: 9a0b...
{"room": "r_1", "sender": "u_42", "creator": "c_7", "amount": 500, "currency": "usd", "message": "great set"}
```

```json
{"tip": {...}, "entry": {...}, "replayed": false,
 "event": {"type": "motom.tip.v1", "id": "tip_...", "tenant": "acme", "room": "r_1",
           "from": "u_42", "to": "c_7", "amount": 500, "currency": "usd",
           "message": "great set", "created_at": "2026-10-06T12:00:00Z"}}
```

`event` is what to show in the room. The same event goes to the configured
`Broadcaster`, after the money has moved. Messages are at most 280 bytes,
with control characters removed.

### Refund

```http
POST /v1/motom/purchases/pur_.../refund
Idempotency-Key: r-1
{"amount": 500}
```

`amount` 0 or omitted refunds the rest. Credits are removed in proportion.
A refund that would need credits the user has already spent is refused
(400). Handle those through a review.

### Creator onboarding and payouts

`POST /creators/{creator}/onboarding` with `country`, `refresh_url` and
`return_url` returns `onboarding_url`, a Stripe-hosted page. Call it again
for a fresh link. `GET /creators/{creator}` returns KYC state, whether they
are payable, open reviews, and earnings per currency (`balance`, `held`,
`available`, `pending`).

`POST /payouts/run` with `{"run": "2026-10-06"}` (default: today, UTC) pays
every payable creator once per run and returns the payout IDs that were
`paid`, `failed` or left `pending`, and `skipped` counted by reason
(`not_onboarded`, `kyc_pending`, `kyc_restricted`, `frozen`,
`under_review`, `below_minimum`, `already_run`, `busy`). Deployments normally run this from the
scheduled `motom payouts run` job instead.

### Tenant config

`PUT /tenant/config` replaces the whole config:

| Field | Default | Meaning |
| --- | --- | --- |
| `tenant_fee_bps` | 0 | Tenant's cut of each tip |
| `packs` | [] | `{id, name, currency, price, bonus}` |
| `tip_min` / `tip_max` | 50 / 50000 | Per-tip bounds |
| `sender_daily_cap` / `room_daily_cap` | 100000 / 2000000 | Rolling 24h tip totals |
| `sender_tips_per_minute` / `room_tips_per_minute` | 10 / 300 | Velocity |
| `purchases_per_hour` | 10 | Per user |
| `review_tip_at` | 25000 | Tips at or above this are reviewed |
| `repeat_pair_per_hour` | 30 | Same sender→creator tips before review |
| `hold_days` | 7 | Days before earnings can be paid out |
| `min_payout` | 1000 | Smallest payout |
| `review_first_payout_at` | 50000 | First payouts at or above this are reviewed |
| `chargeback_liability` | `platform` | Who absorbs spent credits on a chargeback: `platform` or `tenant` |

### Webhook endpoints to register in Stripe

Both point at `https://<host>/v1/motom/webhooks/stripe`:

- **Platform endpoint:** `payment_intent.succeeded`,
  `payment_intent.payment_failed`, `charge.refunded`,
  `charge.dispute.created`, `charge.dispute.closed`, `transfer.reversed`.
  Secret goes in `STRIPE_WEBHOOK_SECRET`.
- **Connect endpoint** ("events on connected accounts"): `account.updated`.
  Secret goes in `STRIPE_CONNECT_WEBHOOK_SECRET`.
