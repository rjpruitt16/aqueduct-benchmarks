#!/bin/sh
# Runs the gateway-failures experiment on Fly with hatchet/deploy/fly/run.sh.
# NOT to be run without approval: it creates paid machines. See PLAN.md.
#
# Usage, from hatchet/: ../gateway-failures/deploy/series.sh <part> [round]
#   (FLY_ORG, FLY_API_TOKEN and HARNESS_IMAGE as for run.sh)
#
#   smoke     two runs at a tenth of the schedule: B with S1 then S4, and A with S3,
#             to check the Fly injections before the full series
#   round     every configuration and scenario once (12 runs); pass the round number
#
# Configurations: A = run.sh D (Envoy -> Hatchet), B = run.sh E (Envoy -> Aquifer ->
# Hatchet), C = run.sh C (Aquifer -> Hatchet). Scenarios: s0 control, s1 Envoy SIGKILL,
# s2 Envoy graceful restart, s3 Hatchet API stopped 5 min, s4 Aquifer SIGKILL.
#
# Bounds: at most PARALLEL runs at once (default 4), at most MAX_RUNS runs started by
# this invocation (default 14), and every app is subject to deploy/fly/reap.sh's
# 4-hour limit. A run with results/<name>/DONE is skipped, so a part can be restarted.
set -u
PART=${1:?usage: series.sh <smoke|round> [round]}
ROUND=${2:-1}
RUN="$(pwd)/deploy/fly/run.sh"
[ -f "$RUN" ] || { echo "run from hatchet/" >&2; exit 2; }
mkdir -p logs
PARALLEL=${PARALLEL:-4}
MAX_RUNS=${MAX_RUNS:-14}
[ -s deploy/fly/harness-image ] && HARNESS_IMAGE=$(cat deploy/fly/harness-image) && export HARNESS_IMAGE

# Shared by every run: the calibrated workload, callers that retry until done,
# Aquifer retrying until Hatchet takes the job, Hatchet's idempotency by document.
export RATE=20 TENANT_MAX_RUNS=50 TRIGGER_SLOTS=20 BUFFER_SIZE=50 BUFFER_FLUSH=25ms ENGINE_DB_CONNS=100
export DRAIN_MAX=${DRAIN_MAX:-60m} MAX_ATTEMPTS=1000000 AQUIFER_MAX_RETRIES=-1 IDEMPOTENCY_TTL=3h
export FORK_API_IMAGE=${FORK_API_IMAGE:-ghcr.io/rjpruitt16/hatchet-aquifer-api:v0.110.8-aqueduct-237971d}

started=0 running=0 pids="" failed=0
# one <config A|B|C> <scenario> <name> [extra env...]
one() {
  cfg=$1 scen=$2 name=$3
  shift 3
  [ -f "results/$name/DONE" ] && return
  if [ "$started" -ge "$MAX_RUNS" ]; then
    echo "MAX_RUNS ($MAX_RUNS) reached; not starting $name" >&2
    return
  fi
  case "$cfg" in
    A) set -- CONFIG_RUN=D API_VM=performance-2x "$@" ;;
    B) set -- CONFIG_RUN=E API_VM=performance-1x PACING=1 "$@" ;;
    C) set -- CONFIG_RUN=C API_VM=performance-2x PACING=1 "$@" ;;
  esac
  # Injection times: defaults for the full schedule, or KILL_AT, STOP_AT and
  # STOP_FOR given among the extra arguments (the smoke part).
  kill_at=900 stop_at=1500 stop_for=300
  for kv in "$@"; do
    case "$kv" in
      KILL_AT=*) kill_at=${kv#KILL_AT=} ;;
      STOP_AT=*) stop_at=${kv#STOP_AT=} ;;
      STOP_FOR=*) stop_for=${kv#STOP_FOR=} ;;
    esac
  done
  case "$scen" in
    s0) ;;
    s1) set -- "$@" ENVOY_KILL_AT=$kill_at ;;
    s2) set -- "$@" ENVOY_DRAIN_AT=$kill_at ENVOY_DRAIN_S=15 ;;
    s3) set -- "$@" API_STOP_AT=$stop_at API_STOP_FOR=$stop_for ;;
    s4) set -- "$@" AQUIFER_KILL_AT=$kill_at ;;
  esac
  # A failed earlier try kept its app; remove it so this try can create it.
  fly apps destroy "aqb-hatchet-$name" -y >/dev/null 2>&1 || true
  mkdir -p "results/$name"
  { echo "config=$cfg"; echo "scenario=$scen"; for kv in "$@"; do echo "$kv"; done; echo "launched=$(date -u +%FT%TZ)"; } > "results/$name/launch.txt"
  # Wait for a free slot.
  # (string operations, not set --, which would overwrite this run's settings)
  while [ "$running" -ge "$PARALLEL" ]; do
    pids=${pids# }
    first=${pids%% *}
    pids=${pids#"$first"}
    wait "$first" || failed=1
    running=$((running - 1))
  done
  (
    cfg_run=""
    for kv in "$@"; do case "$kv" in CONFIG_RUN=*) cfg_run=${kv#CONFIG_RUN=} ;; esac; done
    env "$@" sh "$RUN" "$cfg_run" "$name"
  ) > "logs/$name.log" 2>&1 &
  pids="$pids $!" running=$((running + 1)) started=$((started + 1))
}

echo "== gateway-failures $PART $(date -u +%TZ)"
case "$PART" in
  smoke)
    one B s1 gw-smoke-b-s1 SCALE=0.1 KILL_AT=150 DRAIN_MAX=15m
    one A s3 gw-smoke-a-s3 SCALE=0.1 STOP_AT=150 STOP_FOR=60 DRAIN_MAX=15m
    one C s4 gw-smoke-c-s4 SCALE=0.1 KILL_AT=150 DRAIN_MAX=15m
    ;;
  round)
    r=$ROUND
    for c in A B C; do one "$c" s0 "gw-$(echo $c | tr ABC abc)-s0-$r"; done
    for c in A B; do one "$c" s1 "gw-$(echo $c | tr ABC abc)-s1-$r"; done
    for c in A B; do one "$c" s2 "gw-$(echo $c | tr ABC abc)-s2-$r"; done
    for c in A B C; do one "$c" s3 "gw-$(echo $c | tr ABC abc)-s3-$r"; done
    for c in B C; do one "$c" s4 "gw-$(echo $c | tr ABC abc)-s4-$r"; done
    ;;
  *) echo "unknown part $PART" >&2; exit 2 ;;
esac
for pid in $pids; do wait "$pid" || failed=1; done
for f in logs/gw-*.log; do [ -f "$f" ] && printf '%s: %s\n' "$(basename "$f" .log)" "$(tail -1 "$f")"; done
echo "== gateway-failures $PART done $(date -u +%TZ)"
[ "$failed" = 0 ] || { echo "== some runs failed; see logs/" >&2; exit 1; }
