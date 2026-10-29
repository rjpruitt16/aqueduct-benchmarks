#!/bin/sh
# Stops benchmark apps from running unattended. For every aqb-hatchet-* app
# whose run.sh is no longer alive on this machine:
#
#   - if its driver has finished (/out/DONE), copy the results the way run.sh
#     would (results/<name>, with run.txt from launch.txt) and destroy the app;
#   - otherwise, once the app is older than the grace period, save its Fly
#     logs and destroy it: the run is lost, but it stops costing money.
#
# Any app older than MAX_AGE_MIN is destroyed whatever its state (after the
# same copy, if it finished). Safe to run repeatedly; meant to run every few
# minutes from a loop that outlives this machine's own processes.
#
# Usage, from hatchet/: deploy/fly/reap.sh   (FLY_ORG and FLY_API_TOKEN set)
#   ORPHAN_GRACE_MIN  minutes an unfinished orphan may keep running (default 10)
#   MAX_AGE_MIN       hard limit on any app's age (default 240)
#   DRY_RUN=1         report what would happen, change nothing
set -u
: "${FLY_ORG:?set FLY_ORG}"
: "${FLY_API_TOKEN:?set FLY_API_TOKEN}"
GRACE=${ORPHAN_GRACE_MIN:-10}
MAX_AGE=${MAX_AGE_MIN:-240}
case "$FLY_API_TOKEN" in FlyV1*) auth="$FLY_API_TOKEN" ;; *) auth="Bearer $FLY_API_TOKEN" ;; esac
now=$(date +%s)

act() { if [ "${DRY_RUN:-}" = 1 ]; then echo "  (dry run) $*"; else "$@"; fi; }

for APP in $(fly apps list --json 2>/dev/null | jq -r '.[].Name' | grep '^aqb-hatchet-'); do
  NAME=${APP#aqb-hatchet-}
  OUT=results/$NAME
  alive=""
  pgrep -f "run.sh [BCD] $NAME\$" >/dev/null 2>&1 && alive=1
  machines=$(fly machine list -a "$APP" --json 2>/dev/null) || continue
  created=$(echo "$machines" | jq -r '[.[].created_at] | min // empty')
  # An app with no machines failed in setup: nothing to wait for.
  age=$MAX_AGE
  [ -n "$created" ] && age=$(( (now - $(date -d "$created" +%s)) / 60 ))
  if [ -n "$alive" ] && [ "$age" -lt "$MAX_AGE" ]; then
    continue
  fi
  drv=$(echo "$machines" | jq -r '.[] | select(.name == "driver") | .id')
  done_status=""
  # A stopped driver has nothing to read; ssh to it can hang, so only ask a
  # started one, and not for long.
  if [ -n "$drv" ] && [ "$(echo "$machines" | jq -r '.[] | select(.name == "driver") | .state')" = started ]; then
    done_status=$(timeout 60 fly ssh console -a "$APP" --machine "$drv" -C "cat /out/DONE" 2>/dev/null | tr -d '\r')
  fi
  if [ -z "$done_status" ] && [ "$age" -lt "$GRACE" ] && [ "$age" -lt "$MAX_AGE" ]; then
    echo "$NAME: no run.sh, not finished, ${age} min old; waiting out the grace period"
    continue
  fi
  echo "$NAME: run.sh ${alive:+alive}${alive:-gone}, ${age} min old, driver ${done_status:+finished ($done_status)}${done_status:-not finished}"
  mkdir -p "${OUT:?}"
  if [ -n "$done_status" ] && [ "${DRY_RUN:-}" != 1 ]; then
    for f in summary.json intervals.jsonl postgres.jsonl statements.json driver.log DONE; do
      for try in 1 2 3; do
        timeout 120 fly ssh sftp get -a "$APP" --machine "$drv" "/out/$f" "${OUT:?}/${f:?}" >/dev/null 2>&1 && break
        rm -f "${OUT:?}/${f:?}"; sleep 5
      done
    done
    aq=$(echo "$machines" | jq -r '.[] | select(.name == "aquifer") | .id')
    if [ -n "$aq" ]; then
      timeout 60 fly ssh console -a "$APP" --machine "$aq" -C "wget -qO- http://localhost:8080/metrics" 2>/dev/null \
        | grep -E '^aquifer_(admission_rejected_total|dispatch_attempts_total)' | sed 's/target="[^"]*"/target="hatchet-api"/' > "$OUT/aquifer_metrics.txt" || true
    fi
    echo "$machines" | jq '[.[] | {id, name, region, image: .config.image, guest: .config.guest}]' > "$OUT/machines.json"
    # CPU over the schedule and drain, as run.sh records it.
    if [ -s "$OUT/summary.json" ]; then
      start=$(date -d "$(jq -r '.started' "$OUT/summary.json" | sed 's/\.[0-9]*Z$/Z/')" +%s)
      last=$(jq -r '.phases[-1].end' "$OUT/summary.json" | sed 's/\.[0-9]*Z$/Z/')
      end=$(( $(date -d "$last" +%s) + $(jq -r '.drain_seconds // 0 | floor' "$OUT/summary.json") ))
      q="sum by (instance) (rate(fly_instance_cpu{app=\"$APP\",mode=~\"user|nice|system|irq|softirq\"}[1m])) / 100"
      curl -s -G -H "Authorization: $auth" "https://api.fly.io/prometheus/$FLY_ORG/api/v1/query_range" \
        --data-urlencode "query=$q" --data-urlencode "start=$start" --data-urlencode "end=$end" \
        --data-urlencode "step=30" > "$OUT/cpu.raw.json" 2>/dev/null || true
      jq '[.[] | {(.id): .name}] | add' "$OUT/machines.json" > "$OUT/cpu.names.json"
      jq -n --slurpfile raw "$OUT/cpu.raw.json" --slurpfile names "$OUT/cpu.names.json" \
        '{unit: "cores", machines: [$raw[0].data.result[]? | {machine: ($names[0][.metric.instance] // .metric.instance), values}]}' \
        > "$OUT/cpu.json" 2>/dev/null || true
      rm -f "${OUT:?}/cpu.raw.json" "${OUT:?}/cpu.names.json"
    fi
    {
      [ -f "$OUT/launch.txt" ] && cat "$OUT/launch.txt"
      echo "harness_image=${HARNESS_IMAGE:-unknown}"
      echo "recovered_by=reap.sh $(date -u +%FT%TZ)"
    } > "$OUT/run.txt"
  fi
  [ "${DRY_RUN:-}" = 1 ] || timeout 120 fly logs -a "$APP" --no-tail > "$OUT/fly.log" 2>/dev/null || true
  # A run.sh still alive past the hard limit is stuck; stop it before its app goes.
  if [ -n "$alive" ]; then
    for p in $(pgrep -f "run.sh [BCD] $NAME\$"); do act kill "$p"; done
  fi
  if [ "${DRY_RUN:-}" = 1 ]; then
    echo "  (dry run) would destroy $APP"
  else
    fly apps destroy "$APP" -y >/dev/null 2>&1 && echo "$NAME: app destroyed"
  fi
done
