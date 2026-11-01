#!/usr/bin/env bash
# Re-publish every message from <topic>.dlq back to <topic>. Consumers are
# idempotent (inbox table), so replaying a message twice is safe.
set -euo pipefail
TOPIC="${1:?usage: replay-dlq.sh <topic>, e.g. ride.commands.v1}"
cd "$(dirname "$0")/.."
COUNT=$(docker compose exec -T redpanda rpk topic describe "$TOPIC.dlq" -p | awk 'NR>1 {s += $NF - $(NF-1)} END {print s+0}')
if [ "$COUNT" -eq 0 ]; then echo "$TOPIC.dlq is empty"; exit 0; fi
docker compose exec -T redpanda bash -c "rpk topic consume '$TOPIC.dlq' --offset start --num $COUNT --format '%k\t%v\n' \
  | rpk topic produce '$TOPIC' --format '%k\t%v\n'"
echo "replayed $COUNT message(s) from $TOPIC.dlq to $TOPIC"
