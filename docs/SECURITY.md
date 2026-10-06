# MOTOM security

## Threat model in one paragraph

MOTOM holds no card data and no bank details. Stripe holds those. What
MOTOM holds is the authority to move money: crediting wallets, paying
creators, refunding buyers. The controls below exist to make sure that
authority is only exercised by an authenticated tenant within its scopes,
or by a genuine Stripe event, and that every exercise is balanced,
idempotent and recorded.

## Secrets

- **Nothing secret lives in a file, flag or log.** The binary reads
  `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`,
  `STRIPE_CONNECT_WEBHOOK_SECRET` and `MOTOM_DATABASE_URL` from the
  environment. In deployment, Cloud Run fills these from Secret Manager in
  project `moqom-503700` (`deploy/terraform`), and the runtime service
  account can read only those four secrets.
- Terraform creates secret containers only. Values are added out of band,
  so they never enter Terraform state.
- The binary refuses a value that is not a Stripe secret or restricted key,
  and refuses a live key when `MOTOM_DEV=1`.
- Use a **restricted Stripe key** limited to Checkout Sessions,
  PaymentIntents (write), Refunds (write), Accounts and Account Links
  (Connect, write) and Transfers (write).
- Stripe error messages are logged, not returned to API callers. Internal
  errors return `internal error`.

## Tenant API keys

- Format `mtk_<id>_<secret>`: a 64-bit random ID and a 256-bit random
  secret from `crypto/rand`.
- Only the SHA-256 of the secret is stored. A 256-bit random secret needs
  no slow hash: there is nothing to brute-force from the hash.
- Lookup is by ID, then a **constant-time comparison** of hashes. Unknown
  IDs are compared against a dummy hash so they take the same time as a
  wrong secret.
- The plaintext is shown once, on issue. Revocation takes effect on the
  next request, and a key cannot revoke itself (no accidental lock-out).
- **Scopes** (see API.md) let a tenant give its tipping backend
  `tips:write` without the ability to refund or run payouts.
- The tenant is always taken from the key, never from the request, so a
  key cannot reach another tenant's data.
- Each key is rate limited per replica (default 600 requests a minute).
- Issue and revoke are audited.

## Stripe webhooks

- `Stripe-Signature` is verified as HMAC-SHA256 over `t.payload` with a
  constant-time comparison. Several `v1` signatures and several endpoint
  secrets (platform and Connect) are accepted, which allows secret rotation.
- **Replay protection:** the timestamp must be within 5 minutes, and each
  event ID is processed once (`motom_webhook_events`). If handling fails,
  the ID is forgotten so that Stripe's retry is processed.
- The body is capped at 1 MiB and read before verification. Nothing is
  parsed until the signature checks out.
- Events about objects MOTOM did not create (no `motom_*` metadata, or an
  unknown account or transfer) are acknowledged and ignored.
- MOTOM trusts the amount in Stripe's event, not the client. A payment for
  the wrong amount is flagged, not credited.

## Money integrity

- Double entry: every entry balances per currency. This is checked in Go,
  and again by a deferred Postgres constraint trigger at commit.
- Journal immutability: triggers refuse UPDATE, DELETE and TRUNCATE on
  `motom_entries`, `motom_postings` and `motom_audit`. Corrections are
  reversals.
- No overdraft: wallets, earnings and pending payouts cannot go negative.
  This is enforced in the store under row locks, with optimistic
  concurrency above it. A test fires 60 parallel tips at a wallet that can
  afford 10 and asserts that exactly 10 succeed.
- Idempotency on every write, with mismatch detection.
- Ledger first: no Stripe refund or transfer is requested before its
  balanced entry has committed. Ambiguous outcomes are retried with the
  same Stripe idempotency key, never with a new one.
- Payout gating: KYC verified through Stripe, a payout hold, a minimum
  payout, no open reviews, no freeze.

## HTTP hardening

- Request bodies are capped at 64 KiB and decoded with unknown fields
  rejected.
- The server sets read, header, write and idle timeouts and a header size
  cap.
- Responses carry `Cache-Control: no-store`, `X-Content-Type-Options:
  nosniff` and HSTS.
- TLS terminates at Cloud Run or the load balancer.

## Personal data

MOTOM stores the tenant's **opaque** user, creator and room IDs, plus tip
messages. It never needs names, emails or addresses, and API.md tells
tenants not to send them. Creators' identity documents and bank details go
to Stripe through Stripe-hosted onboarding and are never seen by MOTOM.

## What is still to do before production

- Run the Postgres conformance suite against a real Cloud SQL instance (it
  skips without a database, and this build could not run one).
- Pen-test the API surface, and add the MOTOM endpoints to MOQOM's
  vulnerability scanning.
- Alerting on `motom_webhook_signature_rejected`,
  `motom_webhook_failed`, `motom_tip_record_failed`, payout job failures
  and open `payout_stuck` reviews.
- Decide key rotation cadence for tenants. MOTOM supports overlapping keys
  but does not expire them.
- The per-key rate limiter is per replica. Put a global limit at the load
  balancer (Cloud Armor) if abuse appears.
