# Proposal: MOTOM tools for moqom-mcp

Status: proposal only. Nothing in `moqom-mcp` has been changed.

## Shape

A new `motom.go` beside `billing.go` in `moqom-mcp/internal/mcpserver`,
built on the public `github.com/moqom-cloud/motom/client` package. The tools
are registered only when the server has `MOTOM_URL` and `MOTOM_API_KEY` in
its environment. The MOTOM key is separate from the MOQOM tenant key, and
its scopes bound what an assistant can do. A key for an assistant should not
have `admin`, and usually not `payouts:write` or `refunds:write`.

Conventions follow `billing.go`:

- Snake-case names with a `motom_` prefix.
- Annotations set honestly (`ReadOnlyHint`, `DestructiveHint`,
  `IdempotentHint`, `OpenWorldHint`).
- Anything that moves money or freezes someone requires `"confirm": true`,
  which the tool description says may only be set after the user
  explicitly approves the exact action.
- Money-moving tools take an `idempotency_key`. The server generates one
  if it is missing and returns it, so a retried call reuses it.

## Read-only tools

| Tool | Scope | Returns |
| --- | --- | --- |
| `motom_get_wallet` | wallets:read | Balances, frozen state, and the last N entries for a user |
| `motom_list_packs` | wallets:read | Credit packs on sale |
| `motom_get_purchase` | wallets:read | Status, credits, refunded amount |
| `motom_room_tips` | tips:read | Recent tips in a room |
| `motom_creator_status` | payouts:read | KYC, payable or not and why, earnings held/available/pending |
| `motom_list_payouts` | payouts:read | Payouts by creator and status |
| `motom_list_reviews` | reviews:read | Open (or all) items in the review queue |
| `motom_trial_balance` | admin | Whether the books balance, sums per currency |
| `motom_audit` | admin | Recent audit records, optionally for one subject |

## Tools that act

| Tool | Scope | Confirm | Notes |
| --- | --- | --- | --- |
| `motom_grant_credits` | wallets:write | yes | Tenant pays for the credit; show the amount and user first |
| `motom_onboarding_link` | payouts:write | no | Returns a Stripe-hosted link for the creator to open themselves |
| `motom_resolve_review` | reviews:write | yes | Approve or reject with a note. Rejecting a creator review freezes the creator |
| `motom_freeze_wallet` / `motom_unfreeze_wallet` | reviews:write | yes | Reason required |
| `motom_refund_purchase` | refunds:write | yes | Partial or full. Refused automatically if credits were spent |
| `motom_run_payouts` | payouts:write | yes | Safe to repeat for one run name; returns the report |
| `motom_export_user` | dsar | yes | Returns personal data; the description warns about where the output goes |

## Deliberately left out

- **Spend and tip.** These are user actions in the tenant's app. An
  assistant spending or tipping on a user's behalf invites mistakes nobody
  asked for.
- **Erasure.** Irreversible. Keep it in the tenant's DSAR workflow.
- **Key issue and revoke, and config changes.** Admin actions stay with a
  person using the CLI or API.

## Resources and prompts

- Resource `motom://docs/api`: `motom/docs/API.md`, so the assistant can
  explain errors (for example, what `daily_limit` means).
- Prompt `motom_review_triage`: walk the open review queue, summarise each
  item with the creator's status and recent tips, and propose (not take) a
  decision.

## Tests

Mirror `server_test.go`: an in-process MOTOM (`internal/httpapi` with the
memory store and `stripefake`) behind `httptest`. Assert that each tool
refuses without `confirm`, and that a key without a scope yields a clear
"this key cannot ..." message rather than a raw 403.

The `internal/*` test helpers are not importable from moqom-mcp. Exposing a
small `motomtest` package that starts such a server would be the trivial
enabling change on the MOTOM side.
