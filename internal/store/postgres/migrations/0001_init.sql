-- Copyright 2026 Elliott Wave LLC. All rights reserved.
--
-- MOTOM's schema. Journal entries, postings and the audit log are financial
-- records kept for seven years: triggers below refuse UPDATE, DELETE and
-- TRUNCATE on them, so neither a bug nor a retention sweep can rewrite money
-- history. Corrections are new entries (reversals).

CREATE TABLE motom_tenants (
    id               text PRIMARY KEY,
    currencies       text[] NOT NULL,
    platform_fee_bps integer NOT NULL CHECK (platform_fee_bps BETWEEN 0 AND 10000),
    config           jsonb NOT NULL,
    version          bigint NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL
);

CREATE TABLE motom_keys (
    id         text PRIMARY KEY,
    tenant     text NOT NULL,
    name       text NOT NULL,
    scopes     text[] NOT NULL,
    hash       text NOT NULL,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX motom_keys_tenant ON motom_keys (tenant);

CREATE TABLE motom_accounts (
    id         text PRIMARY KEY,
    tenant     text NOT NULL,
    kind       text NOT NULL,
    owner      text NOT NULL DEFAULT '',
    currency   text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    version    bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL
);
CREATE INDEX motom_accounts_owner ON motom_accounts (tenant, owner);

CREATE TABLE motom_entries (
    seq             bigserial,
    id              text NOT NULL,
    tenant          text NOT NULL,
    kind            text NOT NULL,
    idempotency_key text NOT NULL,
    fingerprint     text NOT NULL,
    reverses        text,
    reference       text NOT NULL DEFAULT '',
    actor           text NOT NULL DEFAULT '',
    memo            text NOT NULL DEFAULT '',
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL,
    CONSTRAINT motom_entries_pkey PRIMARY KEY (id),
    CONSTRAINT motom_entries_seq UNIQUE (seq),
    CONSTRAINT motom_entries_idempotency UNIQUE (tenant, idempotency_key),
    CONSTRAINT motom_entries_reverses UNIQUE (reverses),
    CONSTRAINT motom_entries_reverses_fk FOREIGN KEY (reverses) REFERENCES motom_entries (id)
);
CREATE INDEX motom_entries_tenant_seq ON motom_entries (tenant, seq DESC);

CREATE TABLE motom_postings (
    entry_id   text NOT NULL REFERENCES motom_entries (id),
    ordinal    integer NOT NULL,
    account_id text NOT NULL REFERENCES motom_accounts (id),
    amount     bigint NOT NULL CHECK (amount <> 0),
    currency   text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (entry_id, ordinal),
    UNIQUE (entry_id, account_id)
);
CREATE INDEX motom_postings_account ON motom_postings (account_id, created_at);

-- Defence in depth behind ledger.Entry.Validate: checked at commit, so an
-- entry whose postings do not sum to zero per currency cannot be committed
-- by any code path.
CREATE FUNCTION motom_check_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM motom_postings WHERE entry_id = NEW.entry_id
        GROUP BY currency HAVING SUM(amount) <> 0
    ) THEN
        RAISE EXCEPTION 'motom: entry % does not balance', NEW.entry_id USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER motom_postings_balanced
    AFTER INSERT ON motom_postings DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION motom_check_balanced();

CREATE FUNCTION motom_refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'motom: % is append-only; post a reversal instead', TG_TABLE_NAME USING ERRCODE = '42501';
END $$;

CREATE TRIGGER motom_entries_immutable BEFORE UPDATE OR DELETE ON motom_entries
    FOR EACH ROW EXECUTE FUNCTION motom_refuse_change();
CREATE TRIGGER motom_entries_no_truncate BEFORE TRUNCATE ON motom_entries
    FOR EACH STATEMENT EXECUTE FUNCTION motom_refuse_change();
CREATE TRIGGER motom_postings_immutable BEFORE UPDATE OR DELETE ON motom_postings
    FOR EACH ROW EXECUTE FUNCTION motom_refuse_change();
CREATE TRIGGER motom_postings_no_truncate BEFORE TRUNCATE ON motom_postings
    FOR EACH STATEMENT EXECUTE FUNCTION motom_refuse_change();

CREATE TABLE motom_wallets (
    tenant        text NOT NULL,
    user_id       text NOT NULL,
    frozen        boolean NOT NULL DEFAULT false,
    frozen_reason text NOT NULL DEFAULT '',
    version       bigint NOT NULL,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant, user_id)
);

CREATE TABLE motom_purchases (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    user_id         text NOT NULL,
    pack            text NOT NULL,
    currency        text NOT NULL,
    price           bigint NOT NULL,
    credits         bigint NOT NULL,
    status          text NOT NULL,
    mode            text NOT NULL,
    stripe_intent   text UNIQUE,
    stripe_checkout text NOT NULL DEFAULT '',
    entry_id        text NOT NULL DEFAULT '',
    refunded        bigint NOT NULL DEFAULT 0,
    version         bigint NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX motom_purchases_user ON motom_purchases (tenant, user_id, created_at);

CREATE TABLE motom_tips (
    id         text PRIMARY KEY,
    tenant     text NOT NULL,
    room       text NOT NULL,
    sender     text NOT NULL,
    creator    text NOT NULL,
    currency   text NOT NULL,
    amount     bigint NOT NULL,
    message    text NOT NULL DEFAULT '',
    entry_id   text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX motom_tips_sender ON motom_tips (tenant, sender, created_at);
CREATE INDEX motom_tips_room ON motom_tips (tenant, room, created_at);
CREATE INDEX motom_tips_creator ON motom_tips (tenant, creator, created_at);

CREATE TABLE motom_payees (
    tenant          text NOT NULL,
    creator         text NOT NULL,
    stripe_account  text UNIQUE,
    kyc             text NOT NULL,
    payouts_enabled boolean NOT NULL DEFAULT false,
    disabled_reason text NOT NULL DEFAULT '',
    frozen          boolean NOT NULL DEFAULT false,
    paid_once       boolean NOT NULL DEFAULT false,
    version         bigint NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant, creator)
);

CREATE TABLE motom_payouts (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    creator         text NOT NULL,
    currency        text NOT NULL,
    amount          bigint NOT NULL CHECK (amount > 0),
    status          text NOT NULL,
    run             text NOT NULL,
    entry_id        text NOT NULL,
    settle_entry_id text NOT NULL DEFAULT '',
    stripe_transfer text UNIQUE,
    failure         text NOT NULL DEFAULT '',
    attempts        integer NOT NULL DEFAULT 0,
    version         bigint NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX motom_payouts_tenant ON motom_payouts (tenant, status, created_at);

CREATE TABLE motom_reviews (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    subject     text NOT NULL,
    reason      text NOT NULL,
    detail      text NOT NULL DEFAULT '',
    reference   text NOT NULL DEFAULT '',
    status      text NOT NULL,
    resolved_by text NOT NULL DEFAULT '',
    resolution  text NOT NULL DEFAULT '',
    version     bigint NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE INDEX motom_reviews_queue ON motom_reviews (tenant, status, created_at);
CREATE INDEX motom_reviews_subject ON motom_reviews (tenant, subject);

-- Deduplicates Stripe deliveries. Not a financial record: rows older than
-- Stripe's three-day retry window may be swept.
CREATE TABLE motom_webhook_events (
    id          text PRIMARY KEY,
    kind        text NOT NULL,
    received_at timestamptz NOT NULL
);

CREATE TABLE motom_audit (
    seq     bigserial UNIQUE,
    id      text PRIMARY KEY,
    tenant  text NOT NULL,
    actor   text NOT NULL,
    action  text NOT NULL,
    subject text NOT NULL,
    detail  text NOT NULL DEFAULT '',
    at      timestamptz NOT NULL
);
CREATE INDEX motom_audit_subject ON motom_audit (tenant, subject, seq DESC);
CREATE INDEX motom_audit_tenant ON motom_audit (tenant, seq DESC);

CREATE TRIGGER motom_audit_immutable BEFORE UPDATE OR DELETE ON motom_audit
    FOR EACH ROW EXECUTE FUNCTION motom_refuse_change();
CREATE TRIGGER motom_audit_no_truncate BEFORE TRUNCATE ON motom_audit
    FOR EACH STATEMENT EXECUTE FUNCTION motom_refuse_change();
