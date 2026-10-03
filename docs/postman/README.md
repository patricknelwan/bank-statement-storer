# Postman

Import bca-backend.postman_collection.json and local.postman_environment.json. The environment template contains no credentials. The collection uses Postman Collection v2.1 for Newman 6.2.2.

For real Google connection, open /auth/google/start in a browser, grant the configured owner account access, then copy the one-time application code from the callback page into login_code. Run **Exchange login code** and keep access and refresh tokens in a local sensitive environment only. Never export a populated environment.

For automated HTTP checks, start the service against a dedicated PostgreSQL database whose name ends in _test. Apply migrations. Set DATABASE_URL, BCA_TEST_DATABASE=1, WORKER_INGEST_TOKEN, and optionally POSTMAN_BASE_URL. Run npm ci then npm run postman. The seed command refuses production databases and creates synthetic BCA source jobs and a 60-second login code. The runner creates a temporary environment, verifies positive and negative HTTP cases, then removes that environment. Use POSTMAN_JUNIT=/path/report.xml to retain JUnit results.

The **Manual Only** folder contains browser consent, immediate Gmail sync, operation outcome, eligible retry, and disconnect. It is excluded from the automated run to avoid changing a real mailbox connection. Re-run the seed before a new automated run because successful ingestion is intentionally idempotent.

Set staging base_url and the matching staging test database credentials explicitly. Do not run the synthetic seed against a real owner's database. Google provider behavior and receipt MIME decoding require separate authorized mailbox checks.
