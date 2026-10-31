#!/bin/sh
# Deletes what a dead pilot.sh left behind. Run it from the Sprite's watcher
# every pass.
#
# A RunPod pod named a2a-pilot-<name> is deleted when no pilot.sh for <name>
# is running, or when its deadline (from ~/.a2a-pilot-<name>) is more than
# 15 minutes past. A Fly app aqb-a2a-<name> is destroyed when no pilot.sh for
# <name> is running and its results have been copied (results/<name>/series.log
# ends with SERIES-END), or when it is older than 8 hours.
set -u
export PATH=$HOME/.fly/bin:$PATH FLY_API_TOKEN
FLY_API_TOKEN=$(cat ~/.fly-token)
HERE=$(cd "$(dirname "$0")/.." && pwd)
now=$(date +%s)
running() { pgrep -f "pilot.sh [a-z]* $1\$" >/dev/null; }

if [ -s ~/.runpod-key ]; then
  pods=$(curl -s -H "Authorization: Bearer $(cat ~/.runpod-key)" https://rest.runpod.io/v1/pods)
  if echo "$pods" | jq -e 'type == "array"' >/dev/null 2>&1; then
    echo "$pods" | jq -r '.[] | select(.name | startswith("a2a-pilot-")) | "\(.id) \(.name)"' | while read -r id pname; do
      name=${pname#a2a-pilot-}
      deadline=$(sed -n 's/^deadline=//p' "$HOME/.a2a-pilot-$name" 2>/dev/null)
      if ! running "$name" || [ "$now" -gt $(( ${deadline:-0} + 900 )) ]; then
        code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $(cat ~/.runpod-key)" "https://rest.runpod.io/v1/pods/$id")
        echo "reap: pod $id ($pname) deleted: $code"
      fi
    done
  else
    echo "reap: couldn't read the pod list; skipping pods this pass"
  fi
fi

apps=$(fly apps list --json 2>/dev/null)
if echo "$apps" | jq -e 'type == "array"' >/dev/null 2>&1; then
  echo "$apps" | jq -r '.[] | select(.Name | startswith("aqb-a2a-")) | .Name' | while read -r app; do
    name=${app#aqb-a2a-}
    running "$name" && continue
    created=$(sed -n 's/^created=//p' "$HOME/.a2a-pilot-$name" 2>/dev/null)
    if grep -q SERIES-END "$HERE/results/$name/series.log" 2>/dev/null || [ "$now" -gt $(( ${created:-0} + 8 * 3600 )) ]; then
      fly apps destroy "$app" -y >/dev/null 2>&1 && echo "reap: app $app destroyed"
    else
      echo "reap: app $app has no pilot.sh and uncopied results; leaving it (copy with fly ssh, then rerun)"
    fi
  done
else
  echo "reap: couldn't read the app list; skipping apps this pass"
fi
