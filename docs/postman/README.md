# Postman

Import bca-backend.postman_collection.json and local.postman_environment.json. The environment template contains no credentials. The collection uses Postman Collection v2.1 for Newman 6.2.2.

For real Google connection, open http://127.0.0.1:18080/auth/google/start directly in the same browser that will complete Google consent. Do not send this request from Postman: the OAuth state cookie would stay in Postman while Google redirects your browser, causing `invalid authorization flow`. The sign-in endpoint redirects other hostnames such as `localhost` to the configured host before setting this cookie. Complete consent within five minutes, then copy the one-time application code from the callback page into login_code and run **Exchange login code** within 60 seconds. Keep access and refresh tokens in a local sensitive environment only. Never export a populated environment.

For automated HTTP checks, start the service against a dedicated PostgreSQL database whose name ends in _test. Apply migrations. Set DATABASE_URL, BCA_TEST_DATABASE=1, WORKER_INGEST_TOKEN, and optionally POSTMAN_BASE_URL. Run npm ci then npm run postman. The seed command refuses production databases and creates synthetic BCA source jobs and a 60-second login code. The runner creates a temporary environment, verifies positive and negative HTTP cases, then removes that environment. Use POSTMAN_JUNIT=/path/report.xml to retain JUnit results.

The **Manual Only** folder contains immediate Gmail sync, operation outcome, eligible retry, and disconnect. It is excluded from the automated run to avoid changing a real mailbox connection. Re-run the seed before a new automated run because successful ingestion is intentionally idempotent.

To import a previously unsupported QRIS Payment journal, set `import_id` to its job ID and run **Retry review or unsupported import** in **Manual Only**. Check **Get import outcome** until it says `completed`, then query transactions for the payment date. The result has `receipt_kind: bca_payment` and `payment_to` with the merchant label.

Set staging base_url and the matching staging test database credentials explicitly. Do not run the synthetic seed against a real owner's database. Google provider behavior and receipt MIME decoding require separate authorized mailbox checks.
