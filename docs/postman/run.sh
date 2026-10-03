#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."
: "${DATABASE_URL:?set DATABASE_URL for a dedicated _test database}"
: "${WORKER_INGEST_TOKEN:?set WORKER_INGEST_TOKEN for the test service}"
: "${BCA_TEST_DATABASE:?set BCA_TEST_DATABASE=1}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077
GOTOOLCHAIN=go1.27.1 go run ./cmd/service seed > "$tmp/seed.json"
node docs/postman/build-env.cjs "$tmp/seed.json" "$tmp/environment.json"
./node_modules/.bin/newman run docs/postman/bca-backend.postman_collection.json \
  --environment "$tmp/environment.json" \
  --folder "Health" --folder "Authentication" --folder "Gmail Integration" \
  --folder "Imports" --folder "BCA Ingestion" --folder "Transactions" \
  --folder "Owned Accounts" --folder "Authorization Failures" --folder "End of Run" \
  --reporters cli,junit --reporter-junit-export "${POSTMAN_JUNIT:-$tmp/newman.xml}"
