# BCA email import backend

Go 1.27.1 service for owner-only Gmail receipt discovery, BCA parsing, durable PostgreSQL jobs, protected ingestion, and transaction APIs. The detailed behavior is in [the implementation plan](bca-backend-implementation-plan.md).

## Run locally

1. Create a Google Cloud project, enable the Gmail API, and configure the Google Auth Platform consent screen. Add the mailbox owner as a test user if the app is in Testing. Create an OAuth client of type **Web application** with these authorized redirect URIs:

       http://127.0.0.1:18080/auth/google/callback
       https://developers.google.com/oauthplayground

2. Obtain the owner's stable Google `sub` before starting the app. In [OAuth 2.0 Playground](https://developers.google.com/oauthplayground/), open settings, select **Use your own OAuth credentials**, and enter the new client ID and secret. Authorize the `openid email` scopes while signed in as the mailbox owner, exchange the code, then send a GET request to `https://openidconnect.googleapis.com/v1/userinfo`. Copy the response's `sub` field into `OWNER_GOOGLE_SUB`. Remove the Playground redirect URI from the OAuth client after this step. The app checks this subject on every Google callback.

3. Create `.env` from `.env.example` and restrict its permissions:

       cp .env.example .env
       chmod 600 .env

   Fill `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and `OWNER_GOOGLE_SUB`. Set `DATABASE_URL` to your PostgreSQL connection URI. For a PostgreSQL server on this Linux host using the `finance` database, the form is `postgres://USERNAME:PASSWORD@127.0.0.1:5432/finance?sslmode=disable`; replace the username and password and URL-encode any reserved characters in them. The JDBC `jdbc:` prefix is not accepted. The local Compose file uses host networking so `127.0.0.1:5432` reaches your PostgreSQL server. It does not create a database container; `POSTGRES_PASSWORD` is unused locally.

   Generate and paste independent secrets: `openssl rand -base64 32` for `TOKEN_ENCRYPTION_KEY`, and three separate `openssl rand -hex 32` values for `JWT_SIGNING_SECRET`, `ACCOUNT_HMAC_KEY`, and `WORKER_INGEST_TOKEN`. Leave `TRUST_GMAIL_AUTH_RESULTS=false`. Local setup does not use `TLS_CERT_PATH` or `TLS_KEY_PATH`. Migrations create application tables in the configured database, including `users` and `transactions`; check for existing tables with those names before pointing the app at a populated `finance` database. The previous Docker database volume and its Gmail connection are not copied into your database.

4. Start the local stack and check readiness:

       npm run dev
       curl -f http://127.0.0.1:18080/health/ready

   Open `http://127.0.0.1:18080/auth/google/start` in a browser signed in as the owner. The callback displays a one-time code valid for 60 seconds. Import the [Postman collection and environment](docs/postman/README.md), set `base_url` to `http://127.0.0.1:18080`, paste the code into `login_code`, and run **Exchange login code**. The local service is bound to the host loopback address. Reconnect Gmail after switching databases.

If startup fails, inspect `docker compose --env-file .env -f deploy/compose.local.yml logs migrate service`. A `redirect_uri_mismatch` means the Google client redirect differs from `GOOGLE_REDIRECT_URI`; `owner identity rejected` means `OWNER_GOOGLE_SUB` belongs to a different Google account. The service also needs outbound access to Google's OpenID and Gmail APIs.

## HTTPS deployment

Set `PUBLIC_URL` to the HTTPS origin and `GOOGLE_REDIRECT_URI` to that origin plus `/auth/google/callback`, register the exact redirect URI in Google Cloud, and set `TLS_CERT_PATH` and `TLS_KEY_PATH` to readable certificate and key files. Run `docker compose --env-file .env -f deploy/compose.yml up -d --build`. The migration service runs once before the application, and only the HTTPS proxy publishes ports.

The worker starts immediately, then polls about every 60 seconds. Manual sync returns a durable operation ID; read its outcome at /api/v1/integrations/gmail/sync/{id} or in the integration status. A matching sender is selected using metadata-only From; nonmatching messages are never fetched in full. Automatic import also requires TRUST_GMAIL_AUTH_RESULTS=true after verifying in the authorized mailbox that Gmail adds the trusted receiver result and removes forged results. Until then matching receipts become review outcomes. Successful receipts are imported only when that receiver authentication check passes. Unknown or uncertain messages become review outcomes.

The importer supports BCA transfer receipts and successful QRIS Payment, QRIS Transfer, Flazz Top Up, and BCA Virtual Account receipts with the subject `Internet Transaction Journal`. The QRIS, Flazz, and Virtual Account layouts appear as `bca_payment` with `payment_to` set to the receipt recipient, a masked Flazz card number, or the virtual-account company/product; their initial classification is `unclassified`. Virtual Account receipts record `Pay Amount` as the amount and `Admin Fee` as the fee after checking that their sum equals `Total Payment`; full virtual-account numbers are not saved in transaction fields. Other journal layouts remain unsupported. After updating an existing installation, use `POST /api/v1/imports/{id}/retry` with your bearer token to reprocess an earlier `unsupported` or `needs_review` job, then check its status and the transactions endpoint. The retry fetches the email again from Gmail, so the integration must still be connected.

For unsupported jobs, use `GET /api/v1/imports?state=unsupported` to find IDs, then `GET /api/v1/imports/{id}/inspect` to see the current parser outcome and layout clues. Inspection requires the owner token, refetches only that BCA message after the sender and authentication checks, and does not change the job. The response includes short subject and transaction-type previews plus field names; treat it as private email data. Older `parser_unsupported` jobs can be inspected without retrying them. A `supported_now` result means the updated parser recognizes the email and the job can be retried. Newly processed unsupported jobs also receive a more specific `reason_code` in the import list.

To find the original Gmail message for any import, call `GET /api/v1/imports/{id}/source` with the owner token and paste its `gmail_search` value into Gmail's search bar. This lookup reads only sender-verified message headers and returns subject and date for confirmation; the Gmail integration must still be connected.

## Checks

    make generate
    GOTOOLCHAIN=go1.27.1 go test ./...
    GOTOOLCHAIN=go1.27.1 go vet ./...
    GOTOOLCHAIN=go1.27.1 go mod verify

Set TEST_DATABASE_URL to a migrated isolated PostgreSQL 17 database to run the concurrent ingestion test. The Postman runner uses the same dedicated _test database and never contacts a real mailbox.

## Operations

The public proxy blocks /metrics and /api/v1/webhooks/bca; both remain reachable only inside the private service network. /health/ready checks PostgreSQL and migrations. Disconnect removes usable Google credentials and preserves imported transactions.

Back up the PostgreSQL database with pg_dump, encrypt the archive with age, and copy it off host. Restore with pg_restore into a new database, restore the application encryption key separately, and check integration status before starting the worker. Keep the age identity and encryption key separate from backup archives.

Before production use, verify the actual Gmail partial-response request and sanitized BCA MIME samples, receiver authentication headers, Gmail consent configuration, and recovery after a history checkpoint expires. Pasted receipt text cannot establish live parser accuracy or unattended token lifetime.

## Google acceptance

External OAuth apps in Testing status can receive Gmail-scope refresh tokens that expire after seven days; choose and verify the intended consent-screen setup before relying on unattended polling. See [Google OAuth token expiration](https://developers.google.com/identity/protocols/oauth2#expiration).

A matching Authentication-Results header is not itself proof that the result came from a trusted receiver. Keep TRUST_GMAIL_AUTH_RESULTS=false until the authorized mailbox test confirms Gmail receiver placement and forged-header handling, then enable it deliberately. See [RFC 8601 trust boundaries](https://www.rfc-editor.org/rfc/rfc8601.html).

Check a sanitized example of each real BCA MIME format, the exact metadata-only Gmail request, sender authentication behavior, extended token refresh beyond seven days, outage catch-up, and an off-host encrypted restore before production use.
