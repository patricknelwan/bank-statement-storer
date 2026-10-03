-- name: DatabaseReady :one
SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE c.relname='gmail_sync_operations' AND n.nspname='public' AND c.relkind='r') AS ready;

-- name: CountPendingJobs :one
SELECT count(*)::bigint FROM bca_email_jobs WHERE state IN ('queued','processing','retry_wait');
