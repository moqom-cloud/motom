# MOTOM compliance

This page records how MOTOM is meant to fit MOQOM's compliance program
(`moqom/docs/compliance`) and the questions that need a lawyer. It is not
legal advice. Items marked **For counsel** must be answered before MOTOM
takes live payments.

## Who holds the money

MOTOM uses **Stripe Connect** (Express accounts, separate charges and
transfers):

- Buyers pay Elliott Wave LLC's platform Stripe account through Stripe
  Checkout or Stripe Elements.
- Funds sit in that Stripe balance until they are refunded, or transferred
  to a creator's connected account. Stripe then pays the creator out to
  their bank.
- **Stripe performs KYC** on creators (identity, bank account, sanctions
  screening) through Stripe-hosted onboarding. MOTOM only reads the result
  (`payouts_enabled`, `requirements.disabled_reason`) and will not pay a
  creator Stripe has not verified.
- MOTOM never holds funds in its own bank account and never touches card or
  bank details.

**For counsel: money transmission.** Using Stripe Connect means a licensed
payments provider holds and moves the funds. Whether that is enough for
Elliott Wave LLC to fall outside US state money-transmitter licensing (and
EU/UK payment-services or e-money rules) depends on how the arrangement is
characterised, for example agent-of-the-payee and marketplace exemptions.
Stripe's Connect terms put responsibility for that analysis on the
platform.

**For counsel: stored-value credits.** Viewers pre-pay for credits that
they spend in a tenant's app or tip to creators, and creators can cash tips
out. Please confirm whether these credits are:

1. a closed-loop prepaid access or stored-value product (FinCEN prepaid
   access rules; state gift card and unclaimed property laws, including
   escheat of unused balances and expiry),
2. e-money under the EU EMD2 or UK EMRs, given that they can ultimately
   become money in a third party's (the creator's) bank account, and
3. affected by the fact that credits are denominated 1:1 in currency minor
   units (one credit = one cent), as opposed to a virtual currency at a
   variable rate.

The answer may change product decisions: credit expiry, refunds of unused
credit, whether tips cash out, and per-user limits. MOTOM supports per-user
caps and holds today. It does not implement expiry.

**For counsel: merchant of record and tax.** Who sells the credits: the
tenant (with MOTOM as its processor) or Elliott Wave LLC? That determines
who collects sales tax and VAT on credit sales, who issues receipts, and
whose name appears on card statements. Creator payouts may also need
information reporting (US 1099-K / 1099-NEC, EU DAC7). Stripe Connect can
issue 1099s for Express accounts if enabled.

## PCI DSS

Card data never touches MOTOM. It is entered on Stripe Checkout (hosted) or
in Stripe Elements (Stripe-hosted iframes), so MOTOM's servers never
receive, store or transmit a PAN. That puts a tenant's integration and
MOTOM in scope for **SAQ A**, provided tenants use Checkout or Elements
unmodified and serve their payment pages over TLS. MOTOM receives only
Stripe object IDs and amounts. See `moqom/docs/compliance/PCI-DSS.md`;
MOTOM adds no cardholder data environment.

## Audit log

Every money movement writes an audit record in the same transaction as its
ledger entry. That covers purchases, spends, grants, tips, refunds,
chargebacks, payouts, reversals and returns, each with the actor (`key:<id>`,
`stripe`, `scheduler` or `operator:<name>`), the entry ID and the postings.
The log also records key issue and revoke, config changes, freezes,
review resolutions, fraud-limit hits and DSAR requests. `motom_audit` is
append-only and protected by the same triggers as the journal.
`GET /v1/motom/audit` (admin) reads it.

## Retention

Financial records are kept **seven years and never deleted by sweeps**, in
line with the billing ledger in `moqom/docs/compliance/DATA-RETENTION.md`.

| Data | Table | Retention | Mechanism |
| --- | --- | --- | --- |
| Ledger entries and postings | `motom_entries`, `motom_postings` | 7 years, never edited | Immutability triggers; not in any sweep |
| Audit log | `motom_audit` | 7 years | Immutability triggers |
| Purchases, tips (amounts), payouts | `motom_purchases`, `motom_tips`, `motom_payouts` | 7 years | Not in any sweep |
| Tip message text | `motom_tips.message` | Until erasure request, then blanked | `POST /users/{user}/erase` |
| Reviews | `motom_reviews` | 7 years (fraud and dispute evidence) | Not in any sweep |
| Webhook event IDs | `motom_webhook_events` | Proposed: 90 days | Not yet swept |
| Revoked API keys | `motom_keys` | Proposed: 400 days after revocation, as MOQOM | Not yet swept |

MOQOM's retention sweeper names specific tables, so it never touches
`motom_*`. **Proposed doc change (not made):** add the rows above to
MOQOM's DATA-RETENTION.md, and add the two proposed sweep rules to the
retention module, with `motom_entries`, `motom_postings` and `motom_audit`
listed as protected.

## Data subject requests

- **Access:** `GET /v1/motom/users/{user}/export` (scope `dsar`) returns the
  wallet, every ledger entry touching the user's accounts, purchases, tips
  sent and received, creator status and payouts, and reviews about the
  user. It also names Stripe as the holder of card, payment and identity
  data. This plugs into MOQOM's DSAR process (`moqom/docs/compliance/DSAR.md`).
- **Erasure:** `POST /v1/motom/users/{user}/erase` blanks the user's tip
  messages, which are the only free text MOTOM holds. Financial records are
  kept under the legal-obligation exemption (GDPR Art. 17(3)(b), and
  CCPA's equivalent), and the response lists what was kept and why. They
  are keyed by the tenant's opaque user ID, so once the tenant erases its
  own mapping from that ID to a person, MOTOM's records are pseudonymous.
- Creators' Stripe data is subject to Stripe's own DSAR process.

## Subprocessors

MOTOM adds **Stripe** (payments, Connect, KYC) as a subprocessor for tenants
that use it. It runs on the same GCP project as MOQOM. Update
`moqom/docs/compliance/SUBPROCESSORS.md` and the DPA before launch.

## Consumer protection

- Chargebacks freeze the wallet and open a review; a person decides.
- Refund rules: unused credit is refundable in proportion. Spent credit is
  refused automatically and goes to a person. **For counsel:** check that
  this satisfies cooling-off and refund rights where tenants sell to
  consumers (EU Consumer Rights Directive for digital content, UK CCRs).
- Tip messages are capped at 280 bytes with control characters removed.
  Moderating their content is the tenant's job.
