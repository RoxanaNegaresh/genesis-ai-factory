#!/usr/bin/env bash
#
# Proves end to end that a generated product actually persists data.
#
# This exists because "the code compiles" and "the code works" are different
# claims, and only the second one matters. Everything here runs against a real
# PostgreSQL server: no fakes, no mocks, no in-memory substitute.
#
#   ./scripts/verify-persistence.sh
#
# Requires: go, psql, a reachable PostgreSQL. Set PGPORT/PGHOST/PGUSER to point
# at your own server; the defaults assume a local one on 5432.

set -euo pipefail

PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
DBNAME="${DBNAME:-genesis_verify}"
BRIEF="${BRIEF:-Build a Jira competitor with kanban boards and sprints}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null || true' EXIT

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()  { printf '  \033[32m✔\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✘\033[0m %s\n' "$*"; exit 1; }

say "1. Build the factory"
cd "$ROOT"
make build >/dev/null
ok "genesis-server and genesis built"

say "2. Generate a product"
export GENESIS_DATA_DIR="$WORK/data"
export GENESIS_ADDR="127.0.0.1:8899"
export GENESIS_API="http://127.0.0.1:8899"
export GENESIS_SINGLE_USER=1
export NO_COLOR=1

"$ROOT/bin/genesis-server" >"$WORK/factory.log" 2>&1 &
FACTORY_PID=$!
sleep 3
"$ROOT/bin/genesis" create "$BRIEF" >"$WORK/create.log" 2>&1 \
  || { cat "$WORK/create.log"; die "generation failed"; }
kill "$FACTORY_PID" 2>/dev/null || true
wait "$FACTORY_PID" 2>/dev/null || true

PROJECT="$(find "$GENESIS_DATA_DIR/workspaces" -mindepth 2 -maxdepth 2 -type d | head -1)"
[[ -d "$PROJECT" ]] || die "no workspace produced"
ok "generated $(find "$PROJECT" -type f -not -path '*/.git/*' | wc -l) files"

# The Improver's own verdict is the least flattering source available, which is
# why it is the one worth quoting.
if grep -q "0 high" "$WORK/create.log"; then
  ok "Improver reports 0 high-severity findings"
else
  grep -o "Analysed.*findings" "$WORK/create.log" | tail -1
  die "the generator produced high-severity findings"
fi

say "3. Apply the generated schema"
psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -c "DROP DATABASE IF EXISTS $DBNAME;" >/dev/null
psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -c "CREATE DATABASE $DBNAME;" >/dev/null
psql -q -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -v ON_ERROR_STOP=1 \
  -f "$PROJECT/migrations/0001_init.up.sql" >/dev/null
ok "$(psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';") tables created"

say "4. Build and test the generated project"
cd "$PROJECT/api"
export GOWORK=off
go mod tidy >/dev/null 2>&1
go build ./... || die "the generated project does not compile"
ok "compiles"
go vet ./... || die "go vet rejected the generated project"
ok "go vet clean"

TEST_DATABASE_URL="postgres://$PGUSER@$PGHOST:$PGPORT/$DBNAME?sslmode=disable" \
  go test ./... -count=1 >"$WORK/test.log" 2>&1 || { cat "$WORK/test.log"; die "tests failed"; }
ok "tests pass, including repository tests against the real server"

say "5. Run it and exercise the API"
go build -o "$WORK/server" ./cmd/server
DATABASE_URL="postgres://$PGUSER@$PGHOST:$PGPORT/$DBNAME?sslmode=disable" \
JWT_SECRET="verification-only-secret-of-sufficient-length" \
ADDR="127.0.0.1:8898" "$WORK/server" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
sleep 3

B="http://127.0.0.1:8898"
[[ "$(curl -sf "$B/health" | grep -c ok)" == "1" ]] || die "/health did not answer"
ok "/health"
curl -sf "$B/ready" | grep -q '"database":"ok"' || die "/ready reports no database"
ok "/ready confirms the database is connected"

# Resource routes require a token from v1.2 on, so obtain one first. That an
# anonymous call is refused is asserted explicitly in step 6.
REG="$(curl -sf -X POST "$B/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"crud@example.test","display_name":"CRUD","password":"a-sufficiently-long-password"}')" \
  || die "registration failed"
TOKEN="$(sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p' <<<"$REG")"
[[ -n "$TOKEN" ]] || die "no access token was issued"
AUTH=(-H "Authorization: Bearer $TOKEN")
ok "authenticated as a generated user"

# A create that lands in the database, read back through a different code path.
CREATED="$(curl -sf -X POST "$B/api/v1/projects" "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"key":"VER","name":"Verification","visibility":"team"}')" \
  || die "create failed"
ID="$(printf '%s' "$CREATED" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
[[ -n "$ID" ]] || die "no identifier was returned"
ok "POST   /api/v1/projects → 201 ($ID)"

curl -sf "$B/api/v1/projects/$ID" "${AUTH[@]}" >/dev/null || die "read back failed"
ok "GET    /api/v1/projects/:id → 200"

# The row must genuinely be in PostgreSQL, not merely in the server's memory.
FOUND="$(psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -tAc \
  "SELECT name FROM projects WHERE id = '$ID';")"
[[ "$FOUND" == "Verification" ]] || die "the row is not in the database"
ok "the row is physically present in PostgreSQL"

# A duplicate must be a conflict, not a crash.
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/api/v1/projects" "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"key":"VER","name":"Duplicate","visibility":"team"}')"
[[ "$CODE" == "409" ]] || die "a duplicate returned $CODE, expected 409"
ok "duplicate key → 409"

# A malformed identifier is the caller's mistake, not a server fault.
CODE="$(curl -s -o /dev/null -w '%{http_code}' "$B/api/v1/projects/not-a-uuid" "${AUTH[@]}")"
[[ "$CODE" == "422" ]] || die "a malformed uuid returned $CODE, expected 422"
ok "malformed identifier → 422"

# Archiving must remove the record from reads while retaining the row.
curl -sf -X DELETE "$B/api/v1/projects/$ID" "${AUTH[@]}" -o /dev/null || die "archive failed"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "$B/api/v1/projects/$ID" "${AUTH[@]}")"
[[ "$CODE" == "404" ]] || die "an archived record returned $CODE, expected 404"
STILL="$(psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$DBNAME" -tAc \
  "SELECT count(*) FROM projects WHERE id = '$ID';")"
[[ "$STILL" == "1" ]] || die "archiving destroyed the row instead of soft-deleting it"
ok "DELETE archives: hidden from reads, row retained"

say "6. Authentication"

# Resource routes must reject an anonymous caller. This is the assertion that
# would have caught "every route is public", which shipped from v0.1 to v1.1.
CODE="$(curl -s -o /dev/null -w '%{http_code}' "$B/api/v1/projects")"
[[ "$CODE" == "401" ]] || die "an unauthenticated resource request returned $CODE, expected 401"
ok "resource routes reject anonymous callers"

REG="$(curl -sf -X POST "$B/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"authcheck@example.test","display_name":"Verify","password":"a-sufficiently-long-password"}')" \
  || die "registration failed"

grep -q password_hash <<<"$REG" && die "the response leaked password_hash"
ok "registration does not leak the password hash"

ACCESS="$(sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p' <<<"$REG")"
REFRESH="$(sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p' <<<"$REG")"
[[ -n "$ACCESS" && -n "$REFRESH" ]] || die "no token pair was issued"

CODE="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ACCESS" "$B/api/v1/projects")"
[[ "$CODE" == "200" ]] || die "an authenticated request returned $CODE, expected 200"
ok "a valid token is accepted"

# Wrong password and unknown account must be indistinguishable, or the API is
# an account-enumeration oracle.
WRONG="$(curl -s -X POST "$B/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"authcheck@example.test","password":"the-wrong-password"}')"
UNKNOWN="$(curl -s -X POST "$B/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"nobody@example.test","password":"the-wrong-password"}')"
# Compare the error code, not the whole body: request_id is unique per request
# by design, so raw equality would never hold.
WRONG_CODE="$(sed -n 's/.*"code":"\([^"]*\)".*/\1/p' <<<"$WRONG")"
UNKNOWN_CODE="$(sed -n 's/.*"code":"\([^"]*\)".*/\1/p' <<<"$UNKNOWN")"
[[ -n "$WRONG_CODE" && "$WRONG_CODE" == "$UNKNOWN_CODE" ]] \
  || die "login distinguishes unknown accounts ($UNKNOWN_CODE) from wrong passwords ($WRONG_CODE)"
ok "login does not reveal which accounts exist"

# Rotation, then replay. The replayed token must be refused *and* the token it
# was rotated into must die with it — a revocation that gets rolled back would
# pass the first check and fail the second.
ROTATED="$(curl -sf -X POST "$B/api/v1/auth/refresh" -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}")" || die "refresh failed"
NEXT="$(sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p' <<<"$ROTATED")"
[[ -n "$NEXT" && "$NEXT" != "$REFRESH" ]] || die "the refresh token was not rotated"
ok "refresh rotates the token"

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/api/v1/auth/refresh" \
  -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REFRESH\"}")"
[[ "$CODE" == "401" ]] || die "a replayed refresh token returned $CODE, expected 401"

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/api/v1/auth/refresh" \
  -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$NEXT\"}")"
[[ "$CODE" == "401" ]] || die "the family was not revoked: the rotated token still works ($CODE)"
ok "replaying a retired token revokes the whole family"

say "Verified"
echo "  A generated product was built, migrated, tested and run against"
echo "  PostgreSQL. It stores and retrieves real data, rejects unauthenticated"
echo "  callers, and revokes a refresh-token family on replay."
