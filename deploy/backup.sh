#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${AGE_RECIPIENT:?set an age recipient}"
: "${BACKUP_DIR:?set a protected off-host mounted backup directory}"
case "$BACKUP_DIR" in /*) ;; *) echo 'BACKUP_DIR must be absolute' >&2; exit 1;; esac
umask 077
mountpoint -q "$BACKUP_DIR" || { echo "BACKUP_DIR must be a mounted off-host backup destination" >&2; exit 1; }
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
target="$BACKUP_DIR/bca-$stamp.dump.age"
tmp="$target.tmp"
trap 'rm -f "$tmp"' EXIT
docker compose --env-file .env -f deploy/compose.yml exec -T db pg_dump -U bca -Fc bca | age -r "$AGE_RECIPIENT" > "$tmp"
test -s "$tmp"
mv "$tmp" "$target"
printf '%s\n' "$target"
