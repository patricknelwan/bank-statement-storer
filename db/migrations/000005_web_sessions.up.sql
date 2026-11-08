ALTER TABLE auth_flows ADD COLUMN client_type text NOT NULL DEFAULT 'manual'
 CHECK (client_type IN ('manual','mobile','web'));
ALTER TABLE auth_flows ADD COLUMN return_path text;
UPDATE auth_flows SET client_type='mobile' WHERE client_challenge IS NOT NULL;
ALTER TABLE auth_flows ADD CONSTRAINT web_flow_not_exchangeable CHECK (client_type <> 'web' OR kind='oauth');

CREATE TABLE web_sessions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 secret_hash bytea NOT NULL UNIQUE CHECK (octet_length(secret_hash)=32),
 csrf_token text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz
);
