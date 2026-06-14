#!/usr/bin/env bash
# Run imaptest against a temporary local Mailpit IMAP server.
# Usage: ./run-imaptest.sh [imaptest-args...]

set -euo pipefail

WORKDIR="${TMPDIR:-/tmp}/mailpit-imaptest-$$"
mkdir -p "$WORKDIR"

cleanup() {
    if [[ -n "${MPID:-}" ]]; then
        kill "$MPID" 2>/dev/null || true
        wait "$MPID" 2>/dev/null || true
    fi
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Build mailpit binary
MAILPIT_BIN="$WORKDIR/mailpit"
echo "Building mailpit..."
go build -o "$MAILPIT_BIN" .

# Create IMAP config with a single test user (password: pass)
PASS_HASH=$(printf '%s' 'pass' | sha256sum | awk '{print $1}')
cat > "$WORKDIR/imap.yml" <<EOF
users:
  test@example.com:
    password-hash: "$PASS_HASH"
EOF

cat > "$WORKDIR/users.txt" <<EOF
test@example.com:pass
EOF

# Start mailpit with IMAP on a random local port
IMAP_PORT=1143
SMTP_PORT=1025
HTTP_PORT=8025

"$MAILPIT_BIN" \
    --verbose \
    --db-file "$WORKDIR/mailpit.db" \
    --imap-config "$WORKDIR/imap.yml" \
    --imap "127.0.0.1:$IMAP_PORT" \
    --smtp "127.0.0.1:$SMTP_PORT" \
    --webroot "$WORKDIR/web" \
    > "$WORKDIR/mailpit.log" 2>&1 &
MPID=$!

# Wait for IMAP port to be ready
for i in {1..30}; do
    if bash -c "exec 3<>/dev/tcp/127.0.0.1/$IMAP_PORT" 2>/dev/null; then
        break
    fi
    sleep 0.2
done

# Base args always include the userfile and server.
# Any args you pass on the command line will override these defaults.
BASE_ARGS=(
    userfile="$WORKDIR/users.txt"
    host=127.0.0.1
    port="$IMAP_PORT"
    clients=1
    secs=20
)

echo "Running: imaptest ${BASE_ARGS[*]} $*"
echo "Mailpit log: $WORKDIR/mailpit.log"
imaptest "${BASE_ARGS[@]}" "$@"
