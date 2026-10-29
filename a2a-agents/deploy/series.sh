#!/bin/sh
# The pilot's whole schedule, run inside the pilot machine without anyone
# watching: calibration, then every scenario in every configuration, twice,
# in an interleaved order. Results go to $RESULTS/<run>; $RESULTS/series.log
# says where it is.
#
# It owns the GPU's lifetime: when the schedule ends, fails, or reaches
# DEADLINE, it deletes the RunPod pod (RUNPOD_POD_ID, with RUNPOD_API_KEY).
# The orchestrator on the Sprite deletes it too if this machine goes quiet.
#
# Calibration runs configuration B through a ramp past saturation and
# measures CAPACITY, the workflows per second the stack sustains. If the
# model fixes too little of the corpus (FIX_RATE below MIN_FIX_RATE), the
# series stops there: the comparison would be about the model, not admission.
#
# Scenarios, from CAPACITY (X), each LOAD_S long (480):
#   normal  0.7X, tenants big:3, small-1:1, small-2:1
#   burst   0.5X for a quarter, 3X for an eighth, 0.5X for the rest
#   flood   1.5X, from big:8, small-1:1, small-2:1: the big tenant floods while
#           the small ones send about what they do in normal
#
# Environment: RESULTS (/results), DEADLINE (epoch seconds; required),
# RUNPOD_POD_ID and RUNPOD_API_KEY (empty in rehearsals), LOAD_S (480),
# REPS (2), SCENARIOS (normal burst flood), MIN_FIX_RATE (0.6), CAPACITY
# (skips calibration), CAL_PHASES; and stack.sh's settings.
set -u
RESULTS=${RESULTS:-/results}
: "${DEADLINE:?set DEADLINE}"
LOAD_S=${LOAD_S:-480}
REPS=${REPS:-2}
SCENARIOS=${SCENARIOS:-normal burst flood}
MIN_FIX_RATE=${MIN_FIX_RATE:-0.6}
mkdir -p "$RESULTS"
LOG=$RESULTS/series.log
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$LOG"; }

delete_pod() {
  [ -n "${RUNPOD_POD_ID:-}" ] || return 0
  for i in 1 2 3 4 5 6 7 8 9 10; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $RUNPOD_API_KEY" \
      "https://rest.runpod.io/v1/pods/$RUNPOD_POD_ID")
    case "$code" in 2??|404) log "pod $RUNPOD_POD_ID deleted ($code)"; return 0 ;; esac
    log "deleting pod $RUNPOD_POD_ID answered $code; retrying"
    sleep 15
  done
  log "COULD NOT DELETE POD $RUNPOD_POD_ID"
}
finish() { delete_pod; log "SERIES-END $1"; }
trap 'finish interrupted; exit 1' INT TERM

# The hard limit: whatever is happening, the pod goes at DEADLINE.
(
  while [ "$(date +%s)" -lt "$DEADLINE" ]; do sleep 30; done
  log "deadline reached"
  delete_pod
  pkill -f stack.sh
  pkill -x driver
) &

# run <name> <config> <phases> [tenants]: one stack.sh run, skipped if done.
run() {
  name=$1 cfg=$2 phases=$3 tenants=${4:-big:3,small-1:1,small-2:1}
  if [ -f "$RESULTS/$name/summary.json" ]; then log "$name already done"; return 0; fi
  # A run takes LOAD_S plus up to DRAIN (4m) plus about a minute to start.
  left=$((DEADLINE - $(date +%s)))
  if [ "$left" -lt $((LOAD_S + 300)) ]; then log "skipping $name: ${left}s left before the deadline"; return 1; fi
  log "start $name ($cfg, $phases)"
  rm -rf "${RESULTS:?}/$name"
  OUT=$RESULTS/$name PHASES=$phases TENANTS=$tenants stack.sh "$cfg" > "$RESULTS/$name.stack.log" 2>&1
  rc=$?
  log "end $name rc=$rc $(python3 -c 'import json,sys;s=json.load(open(sys.argv[1]));print(s["outcomes"],"p50",s["p50_ms"],"p99",s["p99_ms"])' "$RESULTS/$name/summary.json" 2>/dev/null)"
  return 0
}

rate() { python3 -c "import sys;print(f'{float(sys.argv[1])*float(sys.argv[2]):.4f}')" "$CAPACITY" "$1"; }

log "series starting; deadline $(date -u -d "@$DEADLINE" +%FT%TZ)"
if [ -z "${CAPACITY:-}" ]; then
  run cal B "${CAL_PHASES:-r1:120:0.05,r2:120:0.1,r3:120:0.2,r4:120:0.4,r5:120:0.8}"
  if [ ! -f "$RESULTS/cal/ledger.jsonl" ]; then finish "calibration failed"; exit 1; fi
  eval "$(python3 -I /usr/local/share/a2a/calibrate.py "$RESULTS/cal/ledger.jsonl")"
  log "calibration: CAPACITY=$CAPACITY workflows/s FIX_RATE=$FIX_RATE per issue: $PER_ISSUE"
  echo "CAPACITY=$CAPACITY FIX_RATE=$FIX_RATE PER_ISSUE=$PER_ISSUE" > "$RESULTS/calibration.txt"
  if python3 -c "import sys;sys.exit(0 if float(sys.argv[1]) < float(sys.argv[2]) else 1)" "$FIX_RATE" "$MIN_FIX_RATE"; then
    finish "fix rate $FIX_RATE below $MIN_FIX_RATE: stopping before the comparison"
    exit 1
  fi
  if python3 -c "import sys;sys.exit(0 if float(sys.argv[1]) <= 0 else 1)" "$CAPACITY"; then
    finish "calibration measured no capacity"
    exit 1
  fi
fi

q=$((LOAD_S / 4)) e=$((LOAD_S / 8))
phases_for() {
  case "$1" in
    normal) echo "normal:$LOAD_S:$(rate 0.7)" ;;
    burst) echo "before:$q:$(rate 0.5),burst:$e:$(rate 3),after:$((LOAD_S - q - e)):$(rate 0.5)" ;;
    flood) echo "flood:$LOAD_S:$(rate 1.5):big=8/small-1=1/small-2=1" ;;
  esac
}
# Interleaved: each configuration goes first, second and third equally often.
orders_1="A B C|B C A|C A B"
orders_2="C B A|A C B|B A C"
rep=1
while [ "$rep" -le "$REPS" ]; do
  if [ $((rep % 2)) -eq 1 ]; then orders=$orders_1; else orders=$orders_2; fi
  k=1
  for s in $SCENARIOS; do
    order=$(echo "$orders" | cut -d'|' -f"$k")
    for cfg in $order; do
      run "$s-$cfg-$rep" "$cfg" "$(phases_for "$s")" || { finish "out of time"; exit 0; }
    done
    k=$((k % 3 + 1))
  done
  rep=$((rep + 1))
done
finish "schedule complete"
