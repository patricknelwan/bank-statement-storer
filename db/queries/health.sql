-- name: DatabaseReady :one
SELECT EXISTS(SELECT 1 FROM information_schema.columns
 WHERE table_schema='public' AND table_name='auth_flows' AND column_name='client_type'
 AND to_regclass('public.gmail_sync_operations') IS NOT NULL
 AND to_regclass('public.web_sessions') IS NOT NULL) AS ready;

-- name: CountPendingJobs :one
SELECT count(*)::bigint FROM bca_email_jobs WHERE state IN ('queued','processing','retry_wait');
