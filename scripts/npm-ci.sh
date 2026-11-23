#!/usr/bin/env bash
# Retry clean installs only when npm reports a transient network failure.
set -euo pipefail

install_log=$(mktemp)
trap 'rm -f "$install_log"' EXIT

for attempt in 1 2 3; do
    printf 'Installing JavaScript dependencies (attempt %s/3)\n' "$attempt"
    if npm ci --prefer-offline --maxsockets=10 \
        --fetch-retries=2 --fetch-retry-factor=2 \
        --fetch-retry-mintimeout=5000 --fetch-retry-maxtimeout=15000 \
        --fetch-timeout=60000 "$@" 2>&1 | tee "$install_log"; then
        exit 0
    else
        install_status=${PIPESTATUS[0]}
    fi

    # A logging failure must not turn a failed pipeline into a successful install.
    if [[ "$install_status" -eq 0 ]]; then
        exit 1
    fi
    if [[ "$attempt" -eq 3 ]] || ! grep -Eq \
        '^npm (ERR!|error) code (ETIMEDOUT|ESOCKETTIMEDOUT|ECONNRESET|ECONNREFUSED|ECONNABORTED|EAI_AGAIN|ENOTFOUND|ENETUNREACH|EHOSTUNREACH|EPIPE|E50[0234])([[:space:]]|$)' \
        "$install_log"; then
        exit "$install_status"
    fi

    delay=$((attempt * 10))
    printf 'npm reported a network failure; retrying in %s seconds.\n' "$delay"
    sleep "$delay"
done
