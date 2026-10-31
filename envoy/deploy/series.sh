#!/bin/sh
# Runs a series: ROUNDS rounds, each one the RUNS side by side, so every
# front door meets the same platform conditions. Waits for each round
# before starting the next.
#
# Usage: deploy/series.sh <series-name>   (FLY_ORG and IMAGE as for run.sh)
#
#   ROUNDS  default 3
#   FIRST   the first round's number (default 1), to add rounds to a series
#   RUNS    mode:regime pairs, default every front door in steady and changing:
#           "envoy:steady aquifer:steady envoy:changing aquifer:changing"
set -u
SERIES=${1:?usage: series.sh <series-name>}
ROUNDS=${ROUNDS:-3}
RUNS=${RUNS:-envoy:steady aquifer:steady envoy:changing aquifer:changing}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p logs
r=${FIRST:-1}
while [ "$r" -lt "$((${FIRST:-1} + ROUNDS))" ]; do
  echo "== round $r $(date -u +%TZ)"
  for run in $RUNS; do
    name=$SERIES-${run%%:*}-${run#*:}-$r
    sh "$HERE/run.sh" "${run%%:*}" "${run#*:}" "$name" > "logs/$name.log" 2>&1 &
  done
  wait
  for f in logs/$SERIES-*-$r.log; do printf '%s: %s\n' "$(basename "$f" .log)" "$(tail -1 "$f")"; done
  r=$((r + 1))
done
echo "== series done $(date -u +%TZ)"
