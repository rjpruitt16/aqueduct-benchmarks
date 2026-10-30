#!/bin/sh
# The controller: runs deploy/pilot.sh from a small Fly machine instead of the
# Sprite, which freezes when no session is open. Results land on the volume at
# /opt/a2a/results.
#
# Pilot: retries creating the GPU pod every 2 minutes, TRIES times (60), until
# RunPod has a matching host, then runs the pilot. Rehearsal: runs once.
# deploy/reap.sh runs every 5 minutes the whole time, and keeps running after
# the pilot until no pod or pilot app is left; then the machine idles for
# KEEP_HOURS (24) so the results can be copied off, and exits.
#
# Environment: KIND (rehearsal|pilot), NAME, IMAGE, plus pilot.sh's own
# (GPU_TYPES, DATA_CENTERS, HOURS, ...). Secrets: RUNPOD_API_KEY, FLY_API_TOKEN.
set -u
: "${KIND:?}" "${NAME:?}" "${IMAGE:?}"
umask 077
printf '%s' "$RUNPOD_API_KEY" > ~/.runpod-key
printf '%s' "$FLY_API_TOKEN" > ~/.fly-token
cd /opt/a2a
mkdir -p results
LOG=results/ctl-$NAME.log
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }

reaper() { while :; do sh deploy/reap.sh >> "$LOG" 2>&1; sleep 300; done; }
reaper &
REAPER=$!

if [ "$KIND" = pilot ]; then
  n=0
  while [ "$n" -lt "${TRIES:-60}" ]; do
    n=$((n + 1))
    sh deploy/pilot.sh pilot "$NAME" > "results/pilot-$NAME.out" 2>&1 < /dev/null
    grep -q "pod not created" "results/$NAME/pilot.log" 2>/dev/null || break
    log "attempt $n: no GPU host"
    rm -f "results/$NAME/pilot.log"
    sleep 120
  done
else
  sh deploy/pilot.sh rehearsal "$NAME" > "results/pilot-$NAME.out" 2>&1 < /dev/null
fi
log "pilot.sh finished"

# Keep reaping until nothing billable is left (pilot.sh has exited, so reap.sh
# deletes whatever it left behind).
until [ "$(curl -s -H "Authorization: Bearer $(cat ~/.runpod-key)" https://rest.runpod.io/v1/pods | jq '[.[]? | select(.name | startswith("a2a-pilot-"))] | length')" = 0 ] \
  && [ "$(/root/.fly/bin/fly apps list --json 2>/dev/null | jq "[.[] | select(.Name == \"aqb-a2a-$NAME\")] | length")" = 0 ]; do
  sleep 60
done
kill "$REAPER"
log "CTL-DONE: no pod or pilot app left"
sleep $(( ${KEEP_HOURS:-24} * 3600 ))
