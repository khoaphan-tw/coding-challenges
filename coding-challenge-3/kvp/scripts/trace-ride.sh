#!/usr/bin/env bash
# What happened to ride X? The state-change lines of all services for that
# ride, in time order (HTTP and message payload lines left out; use
# trace-logs.sh <traceId> for everything).
set -euo pipefail
RIDE="${1:?usage: trace-ride.sh <rideId>}"
cd "$(dirname "$0")/.."

docker compose logs --no-log-prefix --no-color ride-service driver-service dispatch-service 2>/dev/null \
  | grep -F "ride_id=$RIDE" \
  | grep -v -E 'msg="(http request|message produced|message consumed)"' \
  | sort
echo
echo "Durable history: curl -s localhost:8080/rides/$RIDE/history"
echo "Dispatch detail: curl -s localhost:8080/rides/$RIDE/dispatch"

# The whole journey is one trace (docs/observability.md).
TRACE=$(curl -s "${GW:-http://localhost:8080}/rides/$RIDE" | sed -n 's/.*"traceId":"\([0-9a-f]*\)".*/\1/p')
if [ -n "$TRACE" ]; then
  PANES=$(python3 -c 'import json,sys,urllib.parse as u; print(u.quote(json.dumps({"t":{"datasource":"tempo","queries":[{"refId":"A","datasource":{"type":"tempo","uid":"tempo"},"queryType":"traceql","query":sys.argv[1]}],"range":{"from":"now-6h","to":"now"}}},separators=(",",":")),safe=""))' "$TRACE")
  echo "Trace id:        $TRACE"
  echo "Open in Tempo:   http://localhost:3000/explore?schemaVersion=1&orgId=1&panes=$PANES"
  echo "All log lines:   make trace-logs TRACE=$TRACE"
fi
