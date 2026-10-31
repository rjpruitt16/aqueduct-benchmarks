#!/bin/sh
# Writes cpu.json for runs that finished before run.sh saved it, from Fly's
# metrics, which outlive the apps: the same query run.sh makes, over the run's
# schedule and drain, with machine names from its machines.json.
#
# Usage, from hatchet/: ana/backfill-cpu.sh results/<run> ...   (needs FLY_ORG and FLY_API_TOKEN)
set -eu
: "${FLY_ORG:?set FLY_ORG}"
case "${FLY_API_TOKEN:?set FLY_API_TOKEN}" in FlyV1*) auth="$FLY_API_TOKEN" ;; *) auth="Bearer $FLY_API_TOKEN" ;; esac
for run in "$@"; do
  name=$(basename "$run")
  [ -f "$run/cpu.json" ] && { echo "$name: has cpu.json"; continue; }
  [ -f "$run/summary.json" ] && [ -f "$run/machines.json" ] || { echo "$name: no summary or machines"; continue; }
  start=$(date -d "$(sed -n 's/^started=//p' "$run/run.txt")" +%s)
  # The last phase's end, plus the drain.
  last=$(jq -r '.phases[-1].end' "$run/summary.json" | sed 's/\.[0-9]*Z$/Z/')
  end=$(( $(date -d "$last" +%s) + $(jq -r '.drain_seconds // 0 | floor' "$run/summary.json") ))
  app="aqb-hatchet-$name"
  q="sum by (instance) (rate(fly_instance_cpu{app=\"$app\",mode=~\"user|nice|system|irq|softirq\"}[1m])) / 100"
  curl -s -G -H "Authorization: $auth" "https://api.fly.io/prometheus/$FLY_ORG/api/v1/query_range" \
    --data-urlencode "query=$q" --data-urlencode "start=$start" --data-urlencode "end=$end" \
    --data-urlencode "step=30" > "$run/cpu.raw.json"
  jq '[.[] | {(.id): .name}] | add' "$run/machines.json" > "$run/cpu.names.json"
  jq -n --slurpfile raw "$run/cpu.raw.json" --slurpfile names "$run/cpu.names.json" \
    '{unit: "cores", backfilled: true, machines: [$raw[0].data.result[]? | {machine: ($names[0][.metric.instance] // .metric.instance), values}]}' \
    > "$run/cpu.json"
  rm -f "$run/cpu.raw.json" "$run/cpu.names.json"
  echo "$name: $(jq '.machines | length' "$run/cpu.json") machines, $(( (end - start) / 60 )) min"
done
