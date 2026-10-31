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
  # The run's end: its last interval sample, which covers the drain; else the
  # last phase's end (drain_seconds counts from the sustained phase and is -1
  # when the backlog never drained, so it can't be added to it).
  last=$( { [ -s "$run/intervals.jsonl" ] && tail -1 "$run/intervals.jsonl" | jq -r '.at'; } || jq -r '.phases[-1].end' "$run/summary.json")
  end=$(date -d "$(echo "$last" | sed 's/\.[0-9]*Z$/Z/')" +%s)
  app="aqb-hatchet-$name"
  q="sum by (instance) (rate(fly_instance_cpu{app=\"$app\",mode=~\"user|nice|system|irq|softirq\"}[1m])) / 100"
  curl -s -G -H "Authorization: $auth" "https://api.fly.io/prometheus/$FLY_ORG/api/v1/query_range" \
    --data-urlencode "query=$q" --data-urlencode "start=$start" --data-urlencode "end=$end" \
    --data-urlencode "step=30" > "$run/cpu.raw.json"
  # An error answer is not "no machines": leave cpu.json unwritten so a later
  # backfill tries again.
  jq -e '.status == "success"' "$run/cpu.raw.json" >/dev/null 2>&1 || { echo "$name: metrics query failed; no cpu.json written"; rm -f "$run/cpu.raw.json"; continue; }
  jq '[.[] | {(.id): .name}] | add' "$run/machines.json" > "$run/cpu.names.json"
  jq -n --slurpfile raw "$run/cpu.raw.json" --slurpfile names "$run/cpu.names.json" \
    '{unit: "cores", backfilled: true, machines: [$raw[0].data.result[]? | {machine: ($names[0][.metric.instance] // .metric.instance), values}]}' \
    > "$run/cpu.json.tmp" && mv "$run/cpu.json.tmp" "$run/cpu.json"
  rm -f "$run/cpu.raw.json" "$run/cpu.names.json"
  echo "$name: $(jq '.machines | length' "$run/cpu.json") machines, $(( (end - start) / 60 )) min"
done
