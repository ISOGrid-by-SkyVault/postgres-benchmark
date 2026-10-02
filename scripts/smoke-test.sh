#!/bin/sh
# End-to-end check: starts a small benchmark run through the frontend proxy
# and fails unless it completes with the expected topology and no errors.
#
#   ./scripts/smoke-test.sh [base-url] [expected-topology]
#   ./scripts/smoke-test.sh http://localhost:3000 multi-node
set -eu

BASE_URL="${1:-http://localhost:3000}"
EXPECTED_TOPOLOGY="${2:-single}"

# Reads one top-level string or number field from a JSON document on stdin.
field() {
  sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}[,}].*/\1/p" | head -n 1
}

echo "Starting a run on $BASE_URL"
run=$(curl -fsS -X POST "$BASE_URL/api/runs" \
  -H "Content-Type: application/json" \
  -d '{"scale": 1000, "concurrency": 4, "durationSeconds": 1}')
id=$(echo "$run" | field id)
total=$(echo "$run" | field totalActions)
echo "Run $id, $total actions"

status=running
tries=0
while [ "$status" = "running" ]; do
  tries=$((tries + 1))
  if [ "$tries" -gt 120 ]; then
    echo "FAIL: the run did not finish within 4 minutes"
    exit 1
  fi
  sleep 2
  detail=$(curl -fsS "$BASE_URL/api/runs/$id")
  status=$(echo "$detail" | field status)
done

topology=$(echo "$detail" | field topology)
completed=$(echo "$detail" | field completedActions)
echo "Status: $status, topology: $topology, actions: $completed/$total"

if [ "$status" != "completed" ]; then
  echo "FAIL: $(echo "$detail" | field error)"
  exit 1
fi
if [ "$topology" != "$EXPECTED_TOPOLOGY" ]; then
  echo "FAIL: expected topology $EXPECTED_TOPOLOGY"
  exit 1
fi
if [ "$completed" != "$total" ]; then
  echo "FAIL: not every action was recorded"
  exit 1
fi
# "errors":0 on every result
if echo "$detail" | grep -q '"errors":[1-9]'; then
  echo "FAIL: some operations returned errors"
  exit 1
fi

echo "OK"
