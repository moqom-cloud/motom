# MOTOM architecture

## Shape

```
tenant backend ──HTTPS + API key──▶ httpapi ──▶ service ──▶ store (Postgres)
                                                  │  ▲
Stripe ───────signed webhooks─────▶ httpapi ──────┘  │
                                                  ▼  │
                                       stripe client ─┘ (Checkout, PaymentIntents,
                                                         Refunds, Connect, Transfers)
service ──Broadcaster──▶ tenant's room integration ──▶ MOQOM room
```

One stateless Go binary. All state is in Postgres, so replicas scale out
behind a load balancer. A payout run is the same binary run as a scheduled
job.

## The ledger

Package `ledger` is pure. It has no I/O and is shared by both stores.

**Amounts** are `int64` minor units (cents), bounded by `MaxAmount` (10^14) so
that no sum of postings overflows. Each account has exactly one currency.

**Accounts** are named `tenant/kind/owner/currency`. Tenant-wide kinds use
`-` as the owner.

| Kind | Owner | Normal side | Meaning | May go negative |
| --- | --- | --- | --- | --- |
| `stripe_clearing` | – | debit | Money in MOTOM's Stripe balance for this tenant | yes |
| `wallet` | user | credit | Credits a user can spend or tip | **no** |
| `earnings` | creator | credit | What a creator has earned and not yet been paid | **no** |
| `pending_payout` | creator | credit | Transfers started and not yet settled | **no** |
| `fees` | – | credit | MOTOM's platform fees | yes |
| `tenant` | – | credit | Owed to the tenant (its fee and spends, less grants and bonuses it funded) | yes |
| `platform` | – | debit | Losses MOTOM absorbs (chargebacks of spent credit, by default) | yes |

**Postings** are signed: positive is a debit, negative a credit. An
**entry** is a set of postings that sums to zero in each currency. Entries
are immutable. Both stores refuse to update or delete them, and in Postgres
triggers on `motom_entries`, `motom_postings` and `motom_audit` refuse
UPDATE, DELETE and TRUNCATE. A deferred constraint trigger re-checks the
balance of every entry at commit.

**Balances** are always computed from postings, never stored as a number
someone could edit. Each account has a `version` that every posting bumps.
An entry may carry `Expect` (account → version); if any expected version has
moved, the store returns `ErrVersionConflict` and the service re-reads and
retries. This is how parallel tips from one wallet never overdraw it. As a
last line of defence, the store refuses any entry that would take a
non-overdraftable account below zero, checked under row locks taken in
sorted account order.

**Idempotency.** Every entry has an idempotency key that is unique per
tenant. Posting a key again returns the original entry. Posting it with
different postings returns `ErrIdempotencyMismatch`, which is detected by
fingerprint: a hash of kind, reversed entry and sorted postings. The service
also compares the request itself (user, amount, currency, room, creator)
before answering a replay from an existing entry.

**Reversals, not edits.** `ledger.Reverse` builds the mirror entry. An
entry can be reversed at most once (a unique index on `reverses`), and a
reversal cannot itself be reversed.

**Audit.** The store writes an audit record for every entry in the same
transaction as the entry. Actions that are not ledger entries (key issue,
freezes, reviews, rate-limit hits, DSAR requests) are audited separately.

## Money flows

`P` is the pack price, `B` the bonus, `C = P + B` the credits.

| Event | Entry | Postings |
| --- | --- | --- |
| Payment succeeds (webhook) | `purchase` | clearing +P, wallet −C, tenant +B |
| Spend `A` | `spend` | wallet +A, fees −fee, tenant −(A − fee) |
| Grant `A` | `grant` | tenant +A, wallet −A |
| Tip `A` | `tip` | wallet +A, earnings −(A − pf − tf), fees −pf, tenant −tf |
| Refund `R` of a purchase | `refund` | wallet +c, clearing −R, tenant −(c − R) where c is the proportional share of credits |
| Chargeback | `chargeback` | wallet +min(balance, C), liability +shortfall, clearing −P, tenant −B |
| Dispute won | `reversal` of the chargeback | mirror |
| Payout starts | `payout_initiated` | earnings +A, pending_payout −A |
| Transfer settles | `payout_transferred` | pending_payout +A, clearing −A |
| Stripe refuses a transfer | `reversal` of `payout_initiated` | mirror |
| Transfer reversed later | `payout_returned` | clearing +A, earnings −A |

Fees round down (`ledger.Fee`) and the creator gets the remainder, so the
split always conserves the amount. The platform fee is per tenant
(`platform_fee_bps`, set by the operator). The tenant fee is in the tenant's
config (`tenant_fee_bps`).

**Proportional refunds.** Refunding `R` of a purchase removes
`floor(through·C/P) − floor(before·C/P)` credits, where `before` and `through`
are the refunded totals before and after this refund. Partial refunds
therefore add up exactly to a full one. A refund that needs credits the user
has already spent is refused: the money has gone to a creator, so a person
has to decide.

**Chargebacks** take back what is left in the wallet. The shortfall (credits
already spent or tipped) is charged to the `platform` account, or to the
tenant's account if its config says `chargeback_liability: tenant`. Tips
that the disputed credits paid for are **not** clawed back from creators
automatically. The chargeback opens a review, and the reviewer can freeze
the creator if the pattern looks like collusion.

## Ledger first

MOTOM never asks Stripe to move money without a balanced entry already
committed:

1. **Refund:** post the `refund` entry, then call Stripe with an
   idempotency key derived from the entry. If Stripe refuses definitively
   (a 4xx other than 409/429), reverse the entry ("unwind"). If the outcome
   is unknown (timeout, 5xx), leave the entry. A retry with the same key
   reaches Stripe with the same idempotency key.
2. **Payout:** post `payout_initiated`, then create the transfer with the
   payout's idempotency key. On success, post `payout_transferred`. On a
   definitive refusal with no earlier ambiguous attempt, reverse it. If
   there was an ambiguous attempt, the next run retries with the same key.
   Payouts still pending after Stripe's 24-hour idempotency window
   (`IdempotencyWindow` = 23h) stop retrying and open a `payout_stuck`
   review, because a new call could pay twice.
3. **Purchases** move no money out. The `purchase` entry is posted from
   Stripe's `payment_intent.succeeded`, keyed on the intent, so duplicate
   webhooks credit once. If the amount Stripe received does not match the
   pack, the purchase is flagged and no credit is given.

## Payouts

A creator is payable when their Connect Express account reports payouts
enabled and details submitted, the payee is not frozen, and they have no
open review. Each run (by default the UTC date) computes, per creator and
currency:

```
held      = credits to earnings in the last hold_days
available = earnings balance − held
```

It pays `available` if it is at least `min_payout`. Payout IDs derive from
(tenant, creator, currency, run), so rerunning a run never pays twice. A
creator's first payout at or above `review_first_payout_at` waits for a
review.

## Fraud basics

| Check | Effect |
| --- | --- |
| Tips per minute per sender, per room | 429 `rate_limited` |
| Rolling 24h tip total per sender, per room | 429 `daily_limit` |
| Purchases per hour per user | 429 `rate_limited` |
| Tip ≥ `review_tip_at` | `large_tip` review on the creator |
| Same sender→creator more than `repeat_pair_per_hour` | `repeat_pair` review |
| Wallet < 1h old tips ≥ 80% of its balance | `fresh_wallet_drain` review |
| First payout ≥ threshold | `first_payout` review |
| Refund made outside MOTOM (Stripe dashboard) | clawback + `refund_outside_motom` review |
| Paid amount ≠ pack price | purchase flagged, no credit, `amount_mismatch` review |
| Chargeback | clawback, wallet frozen, `chargeback` review |
| Transfer reversed | `transfer_reversed` review |

An open review on a creator holds their payouts. Rejecting it freezes the
creator. Approving a chargeback review unfreezes the wallet. Tip caps are
serialised per sender within a replica, so with N replicas a sender can
exceed a cap by at most N−1 tips. A wallet can never overdraw.

## Store

`store.Store` has two implementations that pass one conformance suite
(`internal/store/storetest`): `memory` (tests, `MOTOM_DEV`) and `postgres`.
The suite includes parallel spends that must never overdraw, parallel
replays that must post once, and random entries that must keep the trial
balance at zero.

Postgres tables (migration `0001_init.sql`, applied under an advisory lock
and recorded in `motom_schema_migrations`):

| Table | Holds |
| --- | --- |
| `motom_tenants` | Tenant, currencies, platform fee, config (JSON), version |
| `motom_keys` | API key ID, tenant, name, scopes, SHA-256 of the secret, revoked_at |
| `motom_accounts` | Account ID, kind, owner, currency, version |
| `motom_entries` | Immutable entries; unique (tenant, idempotency_key), unique reverses |
| `motom_postings` | Immutable postings; balance checked per entry at commit |
| `motom_wallets` | Frozen flag and reason |
| `motom_purchases` | Pack, price, credits, refunded, status, Stripe IDs |
| `motom_tips` | Room, sender, creator, amount, message, entry |
| `motom_payees` | Connect account, KYC state, payouts enabled, frozen |
| `motom_payouts` | Run, amount, status, attempts, transfer, failure |
| `motom_reviews` | Manual review queue |
| `motom_webhook_events` | Stripe event IDs already handled |
| `motom_audit` | Immutable audit log |

## Webhooks

`POST /v1/motom/webhooks/stripe` verifies `Stripe-Signature` (HMAC-SHA256,
5-minute tolerance) against the platform endpoint secret and the Connect
endpoint secret. It records the event ID before handling it, forgets it if
handling fails so Stripe's retry runs again, and ignores events about
objects MOTOM did not create.

| Event | Effect |
| --- | --- |
| `payment_intent.succeeded` | credit the wallet |
| `payment_intent.payment_failed` | mark the purchase failed |
| `charge.refunded` | detect refunds made outside MOTOM |
| `charge.dispute.created` | chargeback |
| `charge.dispute.closed` | reverse the chargeback if won |
| `account.updated` (Connect) | KYC and payout state |
| `transfer.reversed` | return a payout to earnings |

## Integration with MOQOM

MOTOM is a separate module with its own tables (`motom_*`), its own key
prefix (`mtk_`) and its own Stripe objects. No change to `moqom/go` was
needed:

- **Rooms.** `events.Broadcaster` is the hook. A tenant (or a MOQOM
  control-plane adapter, not yet written) implements it to send
  `motom.tip.v1` events into a room. MOTOM is not wired into the relay.
- **Retention.** MOQOM's retention sweeps name specific tables and never
  touch `motom_*`. The immutability triggers would refuse a delete anyway.
- **Billing.** MOQOM's `internal/billing` (usage billing of tenants) is
  untouched. MOTOM reuses its patterns: integer minor units, Stripe calls
  through a small form-encoding client, signed webhooks, hashed API keys.

## Not modelled yet

- Stripe processing fees. `stripe_clearing` is gross. Reconciling it with
  the real Stripe balance (net of fees) needs a balance-transaction import.
- Settling the `tenant` account: paying tenants their share, or netting it
  against MOQOM invoices.
- Currency conversion. Each wallet holds one currency, and a tip must be in
  a currency both sides use.
