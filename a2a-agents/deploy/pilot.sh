#!/bin/sh
# The pilot, or its rehearsal, from the Sprite: creates the GPU pod (pilot
# only) and the pilot machine on Fly, starts deploy/series.sh there, copies
# results every few minutes, and deletes everything at the end.
#
# Usage: deploy/pilot.sh <rehearsal|pilot> <name>
#
# The GPU is deleted by whichever comes first:
#   1. series.sh on the pilot machine, when the schedule ends or fails;
#   2. series.sh's own deadline watcher, at DEADLINE;
#   3. this script, if the pilot machine stops answering for 10 minutes, at
#      DEADLINE + 10 minutes whatever the machine says, or when it exits.
# Every pod this script creates is named a2a-pilot-<name>, and its ID is
# written to $STATE before anything else happens, so deploy/reap.sh can find
# it if this script dies.
#
# Rehearsal: no pod; the stand-in model; short runs (one of each scenario in
# each configuration, 2 minutes each); a 75-minute deadline. It checks the
# deployment, nsjail, the schedule, the copying and the teardown.
#
# Environment: IMAGE (required), FLY_ORG (ezthrottle), HOURS (pilot deadline,
# 5), GPU_TYPES (NVIDIA A40,NVIDIA RTX A6000,NVIDIA L40S), VLLM_IMAGE
# (vllm/vllm-openai:v0.31.0-cu129), MODEL (Qwen/Qwen2.5-7B-Instruct), LLM_MAX (8),
# KEEP=1 (leave the Fly app for debugging; never the pod).
# Reads ~/.runpod-key (pilot) and ~/.fly-token.
set -u
KIND=${1:?usage: pilot.sh <rehearsal|pilot> <name>}
NAME=${2:?usage: pilot.sh <rehearsal|pilot> <name>}
case "$KIND" in rehearsal | pilot) ;; *) echo "first argument: rehearsal or pilot" >&2; exit 2 ;; esac
: "${IMAGE:?set IMAGE}"
export PATH=$HOME/.fly/bin:$PATH FLY_ORG=${FLY_ORG:-ezthrottle}
export FLY_API_TOKEN
FLY_API_TOKEN=$(cat ~/.fly-token)
APP=aqb-a2a-$NAME
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUT=$HERE/results/$NAME
STATE=$HOME/.a2a-pilot-$NAME
mkdir -p "$OUT"
LOG=$OUT/pilot.log
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$LOG"; }
MODEL=${MODEL:-Qwen/Qwen2.5-7B-Instruct}
LLM_MAX=${LLM_MAX:-8}
POD=""

retry() { for try in 1 2 3 4; do "$@" && return 0; sleep 10; done; return 1; }
rp() { curl -s -H "Authorization: Bearer $(cat ~/.runpod-key)" -H 'Content-Type: application/json' "$@"; }

delete_pod() {
  [ -n "$POD" ] || return 0
  for i in 1 2 3 4 5 6 7 8 9 10; do
    code=$(rp -o /dev/null -w '%{http_code}' -X DELETE "https://rest.runpod.io/v1/pods/$POD")
    case "$code" in 2??|404) log "pod $POD deleted ($code)"; echo "deleted" >> "$STATE"; return 0 ;; esac
    log "deleting pod $POD answered $code; retrying"; sleep 15
  done
  log "COULD NOT DELETE POD $POD: delete it in the RunPod console"
}
copy_results() {
  # Through base64: the console isn't guaranteed to pass binary output intact.
  fly ssh console -a "$APP" -C "sh -c 'tar czf - -C /results . | base64'" 2>/dev/null | tr -d '\r' | base64 -d > "$OUT/results.tgz.part" \
    && [ -s "$OUT/results.tgz.part" ] && mv "$OUT/results.tgz.part" "$OUT/results.tgz" \
    && (cd "$OUT" && tar xzf results.tgz) && return 0
  rm -f "$OUT/results.tgz.part"
  return 1
}
teardown() {
  delete_pod
  copy_results && log "results copied to $OUT" || log "final copy failed"
  if [ "${KEEP:-}" = 1 ]; then log "KEEP=1: leaving $APP"; else
    retry fly apps destroy "$APP" -y >/dev/null 2>&1 && log "app $APP destroyed" || log "COULD NOT DESTROY $APP"
  fi
  # Confirm nothing billable is left.
  [ -n "$POD" ] && log "pod check: $(rp -o /dev/null -w '%{http_code}' "https://rest.runpod.io/v1/pods/$POD") (404 = gone)"
  log "app check: $(fly apps list --json 2>/dev/null | jq -r --arg a "$APP" 'map(select(.Name == $a)) | length') left"
}
trap 'teardown; exit 1' INT TERM HUP

if [ "$KIND" = pilot ]; then
  HOURS=${HOURS:-5}
  DEADLINE=$(( $(date +%s) + HOURS * 3600 ))
  VLLM_KEY=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  types=$(echo "${GPU_TYPES:-NVIDIA A40,NVIDIA RTX A6000,NVIDIA L40S}" | jq -R 'split(",")')
  body=$(jq -n --arg name "a2a-pilot-$NAME" --arg image "${VLLM_IMAGE:-vllm/vllm-openai:v0.31.0-cu129}" \
    --arg model "$MODEL" --arg max "$LLM_MAX" --arg key "$VLLM_KEY" --argjson types "$types" '{
      name: $name, imageName: $image, gpuTypeIds: $types, gpuTypePriority: "custom", gpuCount: 1,
      cloudType: "SECURE", interruptible: false, containerDiskInGb: 60, volumeInGb: 0, ports: ["8000/http"],
      dataCenterIds: ["US-IL-1","US-TX-1","US-TX-3","US-TX-4","US-KS-2","US-KS-3","US-GA-1","US-GA-2","US-WA-1","US-CA-2","US-NC-1","US-DE-1","US-MD-1","US-MO-1","US-MO-2","CA-MTL-1","CA-MTL-2","CA-MTL-3"],
      allowedCudaVersions: ["13.0","12.9"],
      dockerStartCmd: [$model,"--max-num-seqs",$max,"--max-model-len","4096","--port","8000","--api-key",$key]}')
  # The image's entrypoint is `vllm serve` (v0.31.0); the flags above are
  # checked against that release's source.
  log "creating the GPU pod"
  resp=$(rp -X POST https://rest.runpod.io/v1/pods -d "$body")
  POD=$(echo "$resp" | jq -r '.id // empty')
  if [ -z "$POD" ]; then log "pod not created: $(echo "$resp" | head -c 500)"; exit 1; fi
  printf 'pod=%s\ncreated=%s\ndeadline=%s\n' "$POD" "$(date +%s)" "$DEADLINE" > "$STATE"
  info=$(rp "https://rest.runpod.io/v1/pods/$POD")
  DC=$(echo "$info" | jq -r '.machine.dataCenterId // .machine.location // empty')
  log "pod $POD: $(echo "$info" | jq -r '.gpu.displayName // .machine.gpuDisplayName // "gpu"') in ${DC:-unknown}, \$$(echo "$info" | jq -r '.costPerHr')/h"
  case "$DC" in
    US-TX*) REGION=dfw ;; US-CA* | US-WA*) REGION=sjc ;; US-NC* | US-DE* | US-MD* | US-GA*) REGION=iad ;; CA-MTL*) REGION=yyz ;; *) REGION=ord ;;
  esac
  LLM_TARGET=https://$POD-8000.proxy.runpod.net
  # The model downloads and loads: give it 25 minutes.
  i=0
  until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $VLLM_KEY" "$LLM_TARGET/v1/models")" = 200 ]; do
    i=$((i + 1))
    if [ "$i" -gt 50 ]; then log "vLLM never came up"; teardown; exit 1; fi
    sleep 30
  done
  log "vLLM ready at $LLM_TARGET after $((i / 2)) minutes"
  SERIES_ENV="LLM_TARGET=$LLM_TARGET LLM_API_KEY=$VLLM_KEY LLM_MODEL=$MODEL RUNPOD_POD_ID=$POD"
else
  DEADLINE=$(( $(date +%s) + 75 * 60 ))
  REGION=${REGION:-ord}
  SERIES_ENV="LOAD_S=120 REPS=1 DRAIN=3m CAL_PHASES=r1:60:0.05,r2:60:0.15,r3:60:0.5"
  printf 'rehearsal\ncreated=%s\ndeadline=%s\n' "$(date +%s)" "$DEADLINE" > "$STATE"
fi

log "creating $APP in $REGION"
retry fly apps create "$APP" --org "$FLY_ORG" >/dev/null 2>&1 || fly apps list --json | jq -e --arg a "$APP" 'any(.[]; .Name == $a)' >/dev/null \
  || { log "app not created"; teardown; exit 1; }
echo "app=$APP" >> "$STATE"
# The machine needs the RunPod key to delete the pod itself. The rehearsal
# sets a dummy one, to check the secret reaches the machine.
if [ "$KIND" = pilot ]; then secret=$(cat ~/.runpod-key); else secret=rehearsal-dummy; fi
echo "RUNPOD_API_KEY=$secret" | fly secrets import -a "$APP" --stage >/dev/null 2>&1 || { log "secret not set"; teardown; exit 1; }
# The machine runs the series, then stays up two hours for the copy, then stops.
envflags=""
for kv in $SERIES_ENV DEADLINE=$DEADLINE LLM_MAX=$LLM_MAX DB_POOL=8 SLOTS=4 CI_RUNNERS=24 CI_DURATION=20s-60s CI_POLL=10s; do
  envflags="$envflags --env $kv"
done
# shellcheck disable=SC2086
# "--" keeps flyctl from reading sh's -c as its own --config.
retry fly machine run "$IMAGE" -a "$APP" -n pilot -r "$REGION" --vm-size performance-4x --vm-memory 8192 \
  --restart no $envflags --detach -- sh -c 'series.sh; sleep 7200' >> "$OUT/flyctl.log" 2>&1 \
  || { log "machine not created"; teardown; exit 1; }
log "series started on $APP ($REGION); deadline $(date -u -d "@$DEADLINE" +%FT%TZ)"
sleep 20
if fly ssh console -a "$APP" -C "sh -c 'test -n \"\$RUNPOD_API_KEY\" && echo key-present'" 2>/dev/null | grep -q key-present; then
  log "the machine has the RunPod key"
else
  log "the machine has no RunPod key: it couldn't delete the pod itself"
  [ "$KIND" = pilot ] && { teardown; exit 1; }
fi

# Watch: copy results every 5 minutes; stop when the series ends.
quiet=0
while :; do
  sleep 300
  if copy_results; then
    quiet=0
    log "$(tail -1 "$OUT/series.log" 2>/dev/null)"
    grep -q SERIES-END "$OUT/series.log" 2>/dev/null && break
  else
    quiet=$((quiet + 5))
    log "pilot machine not answering for $quiet minutes"
    [ "$quiet" -ge 10 ] && { log "machine quiet: deleting the GPU"; delete_pod; }
    [ "$quiet" -ge 30 ] && break
  fi
  [ "$(date +%s)" -gt $((DEADLINE + 600)) ] && { log "past the deadline"; break; }
done
teardown
log "PILOT-DONE"
