#!/usr/bin/env bash
# Scripted walkthrough of the main flows against the running stack (make up).
# Every HTTP call is printed: request (method, URL, headers, body) and
# response (status, notable headers, body).
# Uses its own demo drivers in its own area, so it can be run any number of times.
#   COMPACT=1 make demo   → one-line JSON instead of pretty-printed
set -euo pipefail
GW=${GW:-http://localhost:8080}
DRV=${DRV:-http://localhost:8082}   # driver-service, for demo fixtures only (/internal is not on the gateway)
command -v jq >/dev/null || { echo "jq is required: brew install jq" >&2; exit 1; }
JQ=(jq .); [ -n "${COMPACT:-}" ] && JQ=(jq -c .)

if [ -t 1 ]; then B=$'\033[1m'; C=$'\033[36m'; Y=$'\033[33m'; G=$'\033[32m'; R=$'\033[31m'; D=$'\033[2m'; N=$'\033[0m'
else B= C= Y= G= R= D= N=; fi
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

say()   { printf '\n%s━━ %s ━━%s\n' "$B$C" "$*" "$N"; }
note()  { printf '%s# %s%s\n' "$D" "$*" "$N"; }
pause() { note "waiting ${1:-1}s for async dispatch…"; sleep "${1:-1}"; }

# api METHOD URL [BODY] [HEADER...]
# Prints request + response. Sets $STATUS and $BODY (response).
api() {
  local method=$1 url=$2 body=${3:-}; shift $(( $# < 3 ? $# : 3 ))
  local args=(-s -X "$method" "$url" -H 'Content-Type: application/json' -D "$TMP/h" -o "$TMP/b" -w '%{http_code}')
  local h
  for h in "$@"; do args+=(-H "$h"); done
  [ -n "$body" ] && args+=(-d "$body")

  printf '%s→ %s %s%s\n' "$Y$B" "$method" "$url" "$N"
  for h in "$@"; do printf '%s  %s%s\n' "$Y" "$h" "$N"; done
  [ -n "$body" ] && echo "$body" | "${JQ[@]}" | sed 's/^/  /'

  STATUS=$(curl "${args[@]}")
  BODY=$(cat "$TMP/b")
  local color=$G; [ "$STATUS" -ge 400 ] && color=$R
  printf '%s← %s %s%s' "$color$B" "$STATUS" "$(grep -m1 '^HTTP' "$TMP/h" | cut -d' ' -f3- | tr -d '\r')" "$N"
  local extra; extra=$(grep -i -E '^(Idempotent-Replayed|Content-Type: application/problem)' "$TMP/h" | tr -d '\r' | paste -sd' ' - || true)
  [ -n "$extra" ] && printf '  %s[%s]%s' "$D" "$extra" "$N"
  echo
  if [ -n "$BODY" ]; then echo "$BODY" | "${JQ[@]}" 2>/dev/null | sed 's/^/  /' || echo "  $BODY"; fi
}

# Each run uses its own area (5.5 km apart, beyond the 2 km radius) so old demo drivers never interfere.
RUN=$RANDOM; LAT=$(echo "10.0 + ($RUN % 100) * 0.05" | bc); LNG=106.7700
A="DEMO-A-$RUN"; Bd="DEMO-B-$RUN"; Cd="DEMO-C-$RUN"
north() { echo "$LAT + $1 / 111195" | bc -l | xargs printf '%.6f'; }
RIDE_BODY="{\"passengerId\":\"P001\",\"pickup\":{\"latitude\":$LAT,\"longitude\":$LNG},\"destination\":{\"latitude\":10.7769,\"longitude\":106.7009}}"

say "1. Seed demo drivers (fixture API on driver-service): $A ~300 m, $Bd ~800 m, $Cd ~3 km (outside the 2 km radius)"
api PUT "$DRV/internal/drivers/$A"  "{\"status\":\"AVAILABLE\",\"latitude\":$(north 300),\"longitude\":$LNG}"
api PUT "$DRV/internal/drivers/$Bd" "{\"status\":\"AVAILABLE\",\"latitude\":$(north 800),\"longitude\":$LNG}"
api PUT "$DRV/internal/drivers/$Cd" "{\"status\":\"AVAILABLE\",\"latitude\":$(north 3000),\"longitude\":$LNG}"
pause

KEY="demo-$RUN"
say "2. Passenger requests a ride (AC-01)"
api POST "$GW/rides" "$RIDE_BODY" "Idempotency-Key: $KEY"
RIDE=$(echo "$BODY" | jq -r .rideId)
note "201 returns at once with status REQUESTED; matching happens asynchronously (AC-10)"

say "3. Same request again → same ride, nothing new created (AC-08)"
api POST "$GW/rides" "$RIDE_BODY" "Idempotency-Key: $KEY"
say "   Same key, different body → rejected"
api POST "$GW/rides" "${RIDE_BODY/P001/P002}" "Idempotency-Key: $KEY"

pause
say "4. Nearest driver ($A) received the offer (AC-04, AC-05)"
api GET "$GW/rides/$RIDE"
api GET "$GW/drivers/$A/offer"

say "5. $A rejects → excluded for this ride, ride back to MATCHING, next driver offered (AC-07)"
api POST "$GW/rides/$RIDE/offers/$A/reject"
api POST "$GW/rides/$RIDE/offers/$A/reject"
note "the repeat returns the same result (Idempotent-Replayed)"
pause
api GET "$GW/rides/$RIDE"
api GET "$GW/drivers/$A"

say "6. $Bd accepts, twice: the second is a no-op (AC-08)"
api POST "$GW/rides/$RIDE/offers/$Bd/accept"
api POST "$GW/rides/$RIDE/offers/$Bd/accept"
pause
api GET "$GW/rides/$RIDE"
api GET "$GW/drivers/$Bd"

say "7. Complete the ride → driver AVAILABLE again (AC-02, AC-06)"
api POST "$GW/rides/$RIDE/complete"
pause
api GET "$GW/drivers/$Bd"

say "8. Invalid transition: cancel a COMPLETED ride (AC-06)"
api POST "$GW/rides/$RIDE/cancel"

say "9. Race: two rides at the same moment, only $Bd is in range (AC-09)"
note "taking $A offline so $Bd is the only driver in range"
api PUT "$DRV/internal/drivers/$A" "{\"status\":\"OFFLINE\",\"latitude\":$LAT,\"longitude\":$LNG}"
note "sending both requests in parallel…"
for i in 1 2; do
  curl -s -X POST "$GW/rides" -H 'Content-Type: application/json' -H "Idempotency-Key: race$i-$RUN" -d "$RIDE_BODY" > "$TMP/race$i" &
done
wait
for i in 1 2; do
  printf '%s→ POST %s/rides%s  %s(Idempotency-Key: race%s-%s, sent in parallel)%s\n' "$Y$B" "$GW" "$N" "$D" "$i" "$RUN" "$N"
  "${JQ[@]}" < "$TMP/race$i" | sed 's/^/  /'
done
R1=$(jq -r .rideId < "$TMP/race1"); R2=$(jq -r .rideId < "$TMP/race2")
pause 2
api GET "$GW/rides/$R1"
api GET "$GW/rides/$R2"
api GET "$GW/drivers/$Bd/offer"
note "exactly one ride holds $Bd; the other stays MATCHING with no driver"
note "cleanup: cancelling both race rides"
api POST "$GW/rides/$R1/cancel" >/dev/null; api POST "$GW/rides/$R2/cancel" >/dev/null

say "10. Durable history of the first ride"
api GET "$GW/rides/$RIDE/history"

say "11. What happened to the first ride? (from the logs: make trace-ride RIDE=$RIDE)"
"$(dirname "$0")/trace-ride.sh" "$RIDE"
