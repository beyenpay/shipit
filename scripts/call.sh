#!/usr/bin/env bash
# Calls a shipit webhook, waits for the job to finish and mirrors its result in
# the exit code. Used by action.yml, and handy by hand:
#
#   SHIPIT_URL=http://host:9000 SHIPIT_SECRET=... SHIPIT_PROJECT=web SHIPIT_TAG=v1.2.3 scripts/call.sh
#
# Environment:
#   SHIPIT_URL      base URL of the webhook, e.g. http://203.0.113.10:9000
#   SHIPIT_SECRET   shared secret (same as `secret:` in shipit.yaml)
#   SHIPIT_PROJECT  project name from shipit.yaml
#   SHIPIT_TAG      release tag (required for deploy, optional for rollback)
#   SHIPIT_ACTION   deploy | rollback            (default: deploy)
#   SHIPIT_TIMEOUT  seconds to wait for the job  (default: 600)
#   SHIPIT_POLL     seconds between polls        (default: 3)
set -euo pipefail

: "${SHIPIT_URL:?SHIPIT_URL is required}"
: "${SHIPIT_SECRET:?SHIPIT_SECRET is required}"
: "${SHIPIT_PROJECT:?SHIPIT_PROJECT is required}"
ACTION="${SHIPIT_ACTION:-deploy}"
TAG="${SHIPIT_TAG:-}"
TIMEOUT="${SHIPIT_TIMEOUT:-600}"
POLL="${SHIPIT_POLL:-3}"
URL="${SHIPIT_URL%/}"

case "$ACTION" in
  deploy | rollback) ;;
  *) echo "::error::action must be deploy or rollback, got '$ACTION'" >&2; exit 2 ;;
esac
if [ "$ACTION" = deploy ] && [ -z "$TAG" ]; then
  echo "::error::tag is required for deploy" >&2
  exit 2
fi
for c in curl jq openssl; do
  command -v "$c" > /dev/null 2>&1 || { echo "::error::missing required command: $c" >&2; exit 2; }
done
[[ "$TIMEOUT" =~ ^[0-9]+$ && "$POLL" =~ ^[0-9]+$ && "$POLL" -gt 0 ]] || { echo "::error::timeout/poll must be positive integers" >&2; exit 2; }

echo "::add-mask::$SHIPIT_SECRET"

# request METHOD PATH [BODY]: sets $HTTP_CODE and $RESPONSE. Signature input:
#   timestamp \n METHOD \n path \n body      (HMAC-SHA256, hex)
# curl exit code 7 (cannot connect) is returned as-is so callers can retry.
request() {
  local method="$1" path="$2" body="${3:-}" ts sig out rc
  ts="$(date +%s)"
  sig="$(printf '%s\n%s\n%s\n%s' "$ts" "$method" "$path" "$body" |
    openssl dgst -sha256 -hmac "$SHIPIT_SECRET" -hex | awk '{print $NF}')"
  rc=0
  out="$(curl -sS --max-time 30 -X "$method" "$URL$path" \
    -H "X-Shipit-Timestamp: $ts" -H "X-Shipit-Signature: $sig" \
    -H 'Content-Type: application/json' \
    ${body:+--data-binary "$body"} -w '\n%{http_code}')" || rc=$?
  if [ "$rc" -ne 0 ]; then
    HTTP_CODE=000
    RESPONSE=""
    return "$rc"
  fi
  HTTP_CODE="${out##*$'\n'}"
  RESPONSE="${out%$'\n'*}"
}

fail() { echo "::error::$*" >&2; exit 1; }

describe() { # prints the server's error text if the response is JSON
  jq -r '.error // empty' <<< "$RESPONSE" 2> /dev/null || true
}

body="$(jq -nc --arg p "$SHIPIT_PROJECT" --arg t "$TAG" '{project: $p} + (if $t != "" then {tag: $t} else {} end)')"

echo "shipit: $ACTION $SHIPIT_PROJECT ${TAG:-(previous version)}"
attempt=0
until request POST "/v1/$ACTION" "$body"; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 4 ] || fail "cannot reach $URL after $attempt attempts"
  echo "cannot connect, retrying ($attempt/3)..."
  sleep 5
done
case "$HTTP_CODE" in
  202) ;;
  401) fail "rejected: bad signature or clock skew (check SHIPIT_SECRET; runner and server clocks must be within 5 minutes)" ;;
  429) fail "rate limited by the server, try again in a minute" ;;
  *) fail "server answered HTTP $HTTP_CODE: $(describe)" ;;
esac

job="$(jq -r '.job_id' <<< "$RESPONSE")"
[[ "$job" =~ ^[0-9a-f]+$ ]] || fail "unexpected response: $RESPONSE"
echo "job $job started"

deadline=$(($(date +%s) + TIMEOUT))
printed=0
errors=0
while :; do
  if request GET "/v1/jobs/$job" && [ "$HTTP_CODE" = 200 ]; then
    errors=0
    total="$(jq '.log | length' <<< "$RESPONSE")"
    if [ "$total" -gt "$printed" ]; then
      jq -r --argjson from "$printed" '.log[$from:][]' <<< "$RESPONSE"
      printed="$total"
    fi
    status="$(jq -r '.status' <<< "$RESPONSE")"
    case "$status" in
      success)
        echo "✔ $ACTION of $SHIPIT_PROJECT finished: $(jq -r '.tag' <<< "$RESPONSE") is live"
        exit 0
        ;;
      failed) fail "$ACTION of $SHIPIT_PROJECT failed: $(jq -r '.error' <<< "$RESPONSE")" ;;
    esac
  else
    errors=$((errors + 1))
    # A 404 here means the job is gone (shipit restarted): there is nothing left to wait for.
    [ "$HTTP_CODE" != 404 ] || fail "job $job is unknown to the server (shipit restarted?). Check 'shipit status' on the server."
    [ "$errors" -lt 10 ] || fail "lost contact with the server (last HTTP code $HTTP_CODE)"
    echo "poll failed (HTTP $HTTP_CODE), retrying..."
  fi
  [ "$(date +%s)" -lt "$deadline" ] || fail "timed out after ${TIMEOUT}s; job $job may still be running on the server"
  sleep "$POLL"
done
