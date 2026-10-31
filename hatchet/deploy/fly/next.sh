#!/bin/sh
# Runs the experiments in NEXT.md on Fly, one batch at a time, in parallel
# within a batch. A run with results already in results/<name>/DONE is
# skipped, so the series can be restarted after a failure.
#
# Every producer retries until its document is accepted (MAX_ATTEMPTS very
# large), and Aquifer retries each job until Hatchet takes it
# (AQUIFER_MAX_RETRIES=-1): callers in the wild don't give up.
#
# Usage, from hatchet/: deploy/fly/next.sh <part>   (FLY_ORG and HARNESS_IMAGE as for run.sh)
#
#   smoke     D with a 60 s Postgres outage and C with a resize, at a tenth of the schedule
#   baseline  D three times, C (Patch 2) twice, B once
#   resize    B, D and C: Postgres resized from performance-2x to -4x ten minutes into the sustained overload
#   resize2   the same resize with room to use it: C with Aquifer's and Patch 2's ceilings at
#             twice the calibrated rate, and D with Envoy's limits sized for the bigger database
#   outage    B, D and C: Postgres stopped for 5 and for 15 minutes, ten minutes into the sustained overload
#   smalldb   C twice and B once with Postgres on performance-1x
#   backlog   C with a 5-minute outage, Hatchet advertising 10 minutes of backlog
#             instead of 60 s, on an Aquifer whose fair admission survives a
#             shrinking budget (aquifer#135)
#   backlog2  the rest of the C runs that way, and the 15-minute D outage again
set -u
PART=${1:?usage: next.sh <smoke|baseline|resize|resize2|outage|smalldb|backlog|backlog2>}
HERE=$(cd "$(dirname "$0")" && pwd)
# The harness image these runs use, kept in the repo so the results say which
# driver produced them; HARNESS_IMAGE in the environment is the fallback.
[ -s "$HERE/harness-image" ] && HARNESS_IMAGE=$(cat "$HERE/harness-image") && export HARNESS_IMAGE
mkdir -p logs
# The calibrated settings from TUNING.md.
export RATE=20 TENANT_MAX_RUNS=50 TRIGGER_SLOTS=20 BUFFER_SIZE=50 BUFFER_FLUSH=25ms ENGINE_DB_CONNS=100
export DRAIN_MAX=${DRAIN_MAX:-60m} MAX_ATTEMPTS=1000000 AQUIFER_MAX_RETRIES=-1
export FORK_API_IMAGE=${FORK_API_IMAGE:-ghcr.io/rjpruitt16/hatchet-aquifer-api:v0.110.8-aqueduct-237971d}
# Ten minutes into the sustained overload: steady 600 s + burst 600 s + 600 s.
AT=1800

pids="" names="" failed=0
one() {
  config=$1 name=$2
  shift 2
  # Skip only runs whose driver exited 0; a failed run's DONE holds its status.
  [ "$(cat "results/$name/DONE" 2>/dev/null)" = 0 ] && return
  # Another batch may be running this name right now; leave its app alone.
  if pgrep -f "run.sh [BCD] $name\$" >/dev/null 2>&1; then
    echo "$name: run.sh already running for it; skipped"
    return
  fi
  # A failed earlier try kept its app (its logs are in results/<name>);
  # remove it so this try can create the app again.
  fly apps destroy "aqb-hatchet-$name" -y >/dev/null 2>&1 || true
  # C always runs with Patch 2: Hatchet tells Aquifer how fast to send.
  [ "$config" = C ] && set -- PACING=1 "$@"
  # What this run was launched with, so deploy/fly/reap.sh can write its
  # run.txt if run.sh dies before it does.
  mkdir -p "results/$name"
  { echo "config=$config"; for kv in "$@"; do echo "$kv"; done; echo "launched=$(date -u +%FT%TZ)"; } > "results/$name/launch.txt"
  env "$@" sh "$HERE/run.sh" "$config" "$name" > "logs/$name.log" 2>&1 &
  pids="$pids $!" names="$names $name"
}
batch_done() {
  for pid in $pids; do wait "$pid" || failed=1; done
  for n in $names; do printf '%s: %s\n' "$n" "$(tail -1 "logs/$n.log")"; done
  pids="" names=""
}

echo "== $PART $(date -u +%TZ)"
case "$PART" in
  smoke)
    one D nx-smoke-d SCALE=0.1 OUTAGE_AT=150 OUTAGE_FOR=60 DRAIN_MAX=15m
    one C nx-smoke-c SCALE=0.1 RESIZE_AT=120 RESIZE_TO=performance-4x DRAIN_MAX=15m
    ;;
  baseline)
    for r in 1 2 3; do one D "nx-d-$r"; done
    for r in 1 2; do one C "nx-c-$r"; done
    one B nx-b-1
    ;;
  resize)
    for c in B D C; do
      for r in 1 2; do
        one "$c" "nx-resize-$(echo "$c" | tr BCD bcd)-$r" RESIZE_AT=$AT RESIZE_TO=performance-4x
      done
    done
    ;;
  resize2)
    for r in 1 2; do
      one C "nx-resize-c40-$r" RESIZE_AT=$AT RESIZE_TO=performance-4x AQUIFER_RPS=40 PACING_MAX_RPS=40
      one D "nx-resize-d40-$r" RESIZE_AT=$AT RESIZE_TO=performance-4x GLOBAL_RPS=40 TENANT_RPS=30
    done
    ;;
  outage)
    for c in B D C; do
      for m in 5 15; do
        one "$c" "nx-out$m-$(echo "$c" | tr BCD bcd)" OUTAGE_AT=$AT OUTAGE_FOR=$((m * 60))
      done
    done
    ;;
  smalldb)
    for r in 1 2; do one C "nx-small-c-$r" PG_SIZE=performance-1x; done
    one B nx-small-b-1 PG_SIZE=performance-1x
    ;;
  backlog)
    one C nx-bl-out5-c-1 OUTAGE_AT=$AT OUTAGE_FOR=300 PACING_BACKLOG_SECONDS=600
    ;;
  backlog2)
    one C nx-bl-out5-c-2 OUTAGE_AT=$AT OUTAGE_FOR=300 PACING_BACKLOG_SECONDS=600
    one C nx-bl-out15-c OUTAGE_AT=$AT OUTAGE_FOR=900 PACING_BACKLOG_SECONDS=600
    for r in 1 2; do one C "nx-bl-c-$r" PACING_BACKLOG_SECONDS=600; done
    one D nx-out15-d-2 OUTAGE_AT=$AT OUTAGE_FOR=900
    ;;
  *) echo "unknown part $PART" >&2; exit 2 ;;
esac
batch_done
echo "== $PART done $(date -u +%TZ)"
[ "$failed" = 0 ] || { echo "== some runs failed; see logs/" >&2; exit 1; }
