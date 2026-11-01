#!/usr/bin/env bash
# Every log line of one trace (= one ride journey) from all services, in time
# order: HTTP request/response bodies and produced/consumed message values included.
#   trace-logs.sh <traceId>        (POST /rides and GET /rides/<rideId> return traceId)
set -euo pipefail
TRACE="${1:?usage: trace-logs.sh <traceId>}"
cd "$(dirname "$0")/.."
# Lines start with their timestamp (time=… or {"time":…), so sort orders them.
docker compose logs --no-log-prefix --no-color ride-service driver-service dispatch-service 2>/dev/null \
  | grep -F "trace_id=$TRACE" | sort
