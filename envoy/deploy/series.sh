#!/bin/sh
# Runs the full series: ROUNDS rounds, each one run of envoy and aquifer in
# both regimes, side by side, so the two front doors meet the same platform
# conditions. Waits for each round before starting the next.
#
# Usage: deploy/series.sh <series-name>   (FLY_ORG and IMAGE as for run.sh)
set -u
SERIES=${1:?usage: series.sh <series-name>}
ROUNDS=${ROUNDS:-3}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p logs
r=1
while [ "$r" -le "$ROUNDS" ]; do
  echo "== round $r $(date -u +%TZ)"
  for regime in steady changing; do
    for mode in envoy aquifer; do
      name=$SERIES-$mode-$regime-$r
      sh "$HERE/run.sh" "$mode" "$regime" "$name" > "logs/$name.log" 2>&1 &
    done
  done
  wait
  for f in logs/$SERIES-*-$r.log; do printf '%s: %s\n' "$(basename "$f" .log)" "$(tail -1 "$f")"; done
  r=$((r + 1))
done
echo "== series done $(date -u +%TZ)"
