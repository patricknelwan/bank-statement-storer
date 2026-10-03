CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 google_sub text NOT NULL UNIQUE,
 email text NOT NULL,
 timezone text NOT NULL DEFAULT 'Asia/Jakarta',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 family_id uuid NOT NULL,
 refresh_hash bytea NOT NULL UNIQUE,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE auth_flows (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 kind text NOT NULL CHECK (kind IN ('oauth','login_code')),
 secret_hash bytea NOT NULL UNIQUE,
 nonce text,
 pkce_verifier text,
 user_id uuid REFERENCES users(id),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE TABLE gmail_integrations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL UNIQUE REFERENCES users(id),
 mailbox_id text NOT NULL UNIQUE,
 encrypted_refresh_token bytea,
 key_version smallint NOT NULL DEFAULT 1,
 status text NOT NULL CHECK (status IN ('connected','disconnected','reconnect_required')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE gmail_sync_state (
 integration_id uuid PRIMARY KEY REFERENCES gmail_integrations(id),
 history_id text,
 coverage_start timestamptz NOT NULL,
 last_attempt timestamptz,
 last_success timestamptz,
 lease_until timestamptz,
 lease_generation bigint NOT NULL DEFAULT 0,
 recovery_status text NOT NULL DEFAULT 'initial'
);
CREATE TABLE bca_email_jobs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 integration_id uuid NOT NULL REFERENCES gmail_integrations(id),
 message_id text NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','processing','retry_wait','completed','ignored','unsupported','needs_review','failed')),
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,
 parser_version text,
 reason_code text,
 encrypted_source bytea,
 delivery_payload jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (integration_id, message_id)
);
CREATE INDEX bca_jobs_due_idx ON bca_email_jobs(next_attempt_at) WHERE state IN ('queued','retry_wait','processing');
CREATE TABLE transactions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 integration_id uuid NOT NULL REFERENCES gmail_integrations(id),
 bank_reference text NOT NULL,
 receipt_kind text NOT NULL CHECK (receipt_kind IN ('bca_transfer','interbank_transfer')),
 classification text NOT NULL DEFAULT 'unclassified' CHECK (classification IN ('unclassified','expense','internal_transfer')),
 occurred_at timestamptz NOT NULL,
 source_local_time text NOT NULL,
 source_timezone text NOT NULL,
 amount_minor bigint NOT NULL CHECK (amount_minor > 0),
 fee_minor bigint CHECK (fee_minor >= 0),
 currency text NOT NULL CHECK (currency = 'IDR'),
 source_account_alias text NOT NULL,
 beneficiary_bank text,
 beneficiary_account_masked text NOT NULL,
 beneficiary_match_token bytea,
 parser_version text NOT NULL,
 note text NOT NULL DEFAULT '',
 version integer NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (user_id, bank_reference, source_account_alias, receipt_kind)
);
CREATE TABLE transaction_sources (
 transaction_id uuid NOT NULL REFERENCES transactions(id),
 job_id uuid NOT NULL UNIQUE REFERENCES bca_email_jobs(id),
 PRIMARY KEY (transaction_id, job_id)
);
CREATE TABLE owned_accounts (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 bank text NOT NULL,
 match_token bytea NOT NULL,
 masked_identifier text NOT NULL,
 label text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (user_id, bank, match_token)
);
CREATE TABLE transaction_audit (
 id bigserial PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 transaction_id uuid NOT NULL REFERENCES transactions(id),
 action text NOT NULL,
 before_values jsonb NOT NULL,
 after_values jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
