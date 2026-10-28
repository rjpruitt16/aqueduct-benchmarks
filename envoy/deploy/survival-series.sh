#!/bin/sh
# Runs the survival series (see DESIGN-survival.md): envoy, envoy-max and
# aquifer at each scale delay, two delays at a time, for ROUNDS rounds; then
# Aquifer's two variants at a delay of 5 minutes, both rounds side by side.
# Waits for each batch before starting the next.
#
# PART=client instead runs the client comparison: Aquifer with the series'
# backoff client (the control) and with its own client (HOLD=1), at delays of
# 3 and 5 minutes, one round at a time.
#
# Usage: deploy/survival-series.sh <series-name>   (FLY_ORG and IMAGE as for run.sh)
#
#   ROUNDS    default 2
#   DELAYS    default "1 3 5 10", run in pairs
#   SETUPS    default "envoy envoy-max aquifer"
#   VARIANTS  0 to skip the variants
#   PART      main (default) or client
set -u
SERIES=${1:?usage: survival-series.sh <series-name>}
ROUNDS=${ROUNDS:-2}
DELAYS=${DELAYS:-1 3 5 10}
SETUPS=${SETUPS:-envoy envoy-max aquifer}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p logs

# one <name> <mode> [env...] runs one run.sh in the background.
# A run with results already in results/<name>/run.txt is skipped, so the
# series can be restarted after a failure.
one() {
  name=$1 mode=$2
  shift 2
  [ -f "results/$name/run.txt" ] && return
  env "$@" sh "$HERE/run.sh" "$mode" survival "$name" > "logs/$name.log" 2>&1 &
}
batch_done() {
  wait
  for f in "$@"; do printf '%s: %s\n' "$f" "$(tail -1 "logs/$f.log")"; done
}

if [ "${PART:-main}" = client ]; then
  r=1
  while [ "$r" -le "$ROUNDS" ]; do
    names=""
    for d in 3 5; do
      one "$SERIES-aquifer-ctl-d$d-$r" aquifer SCALE_DELAY="$d"
      one "$SERIES-aquifer-hold-d$d-$r" aquifer SCALE_DELAY="$d" HOLD=1
      names="$names $SERIES-aquifer-ctl-d$d-$r $SERIES-aquifer-hold-d$d-$r"
    done
    echo "== client round $r $(date -u +%TZ)"
    # shellcheck disable=SC2086
    batch_done $names
    r=$((r + 1))
  done
  echo "== series done $(date -u +%TZ)"
  exit 0
fi

r=1
while [ "$r" -le "$ROUNDS" ]; do
  set -- $DELAYS
  while [ $# -gt 0 ]; do
    names=""
    for d in $1 ${2:-}; do
      for s in $SETUPS; do
        name=$SERIES-$s-d$d-$r
        one "$name" "$s" SCALE_DELAY="$d"
        names="$names $name"
      done
    done
    echo "== round $r, delays $1 ${2:-} $(date -u +%TZ)"
    # shellcheck disable=SC2086
    batch_done $names
    shift
    [ $# -gt 0 ] && shift
  done
  r=$((r + 1))
done

if [ "${VARIANTS:-1}" != 0 ]; then
  echo "== variants $(date -u +%TZ)"
  names=""
  r=1
  while [ "$r" -le "$ROUNDS" ]; do
    one "$SERIES-aquifer-noheaders-d5-$r" aquifer SCALE_DELAY=5 ADVERTISE=none
    one "$SERIES-aquifer-nokey-d5-$r" aquifer SCALE_DELAY=5 NO_KEY=1
    names="$names $SERIES-aquifer-noheaders-d5-$r $SERIES-aquifer-nokey-d5-$r"
    r=$((r + 1))
  done
  # shellcheck disable=SC2086
  batch_done $names
fi
echo "== series done $(date -u +%TZ)"
