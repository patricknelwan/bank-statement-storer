CREATE TABLE gmail_sync_operations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 integration_id uuid NOT NULL REFERENCES gmail_integrations(id),
 status text NOT NULL CHECK (status IN ('queued','running','completed','failed')),
 reason_code text,
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz
);
CREATE INDEX gmail_sync_operations_owner_idx ON gmail_sync_operations(user_id,created_at DESC);
CREATE UNIQUE INDEX gmail_sync_operations_one_active ON gmail_sync_operations(integration_id) WHERE status IN ('queued','running');
