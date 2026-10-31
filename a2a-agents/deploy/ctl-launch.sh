#!/bin/sh
# Starts the controller (deploy/ctl.sh) on Fly: app aqb-ctl-a2a, the RunPod key
# and Fly token as secrets, a 1 GB volume for results, one small machine.
# The app name must not start with aqb-a2a-: deploy/reap.sh destroys those.
#
# Usage: IMAGE=... deploy/ctl-launch.sh <rehearsal|pilot> <name> [KEY=VALUE ...]
# Extra KEY=VALUE pairs go to the controller's environment (GPU_TYPES, ...).
# Copy results off: fly ssh console -a aqb-ctl-a2a -C "tar czf - -C /opt/a2a/results ."
# When done: fly apps destroy aqb-ctl-a2a -y
set -eu
KIND=${1:?usage: ctl-launch.sh <rehearsal|pilot> <name> [KEY=VALUE ...]}
NAME=${2:?usage: ctl-launch.sh <rehearsal|pilot> <name> [KEY=VALUE ...]}
shift 2
: "${IMAGE:?set IMAGE}"
export PATH=$HOME/.fly/bin:$PATH FLY_API_TOKEN
FLY_API_TOKEN=$(cat ~/.fly-token)
APP=aqb-ctl-a2a
REGION=${CTL_REGION:-ord}

fly apps list --json | jq -e --arg a "$APP" 'any(.[]; .Name == $a)' >/dev/null \
  || fly apps create "$APP" --org "${FLY_ORG:-ezthrottle}" >/dev/null
printf 'RUNPOD_API_KEY="%s"\nFLY_API_TOKEN="%s"\n' "$(cat ~/.runpod-key)" "$(cat ~/.fly-token)" \
  | fly secrets import -a "$APP" --stage >/dev/null
fly volumes list -a "$APP" --json | jq -e 'length > 0' >/dev/null \
  || fly volumes create a2a_results -a "$APP" -r "$REGION" -s 1 -y >/dev/null
# Each extra pair becomes its own --env argument (values may hold spaces).
for kv in "$@"; do set -- "$@" --env "$kv"; shift; done
fly machine run "$IMAGE" -a "$APP" -n "ctl-$NAME" -r "$REGION" --vm-size shared-cpu-1x --vm-memory 512 \
  --restart no --volume a2a_results:/opt/a2a/results --env "KIND=$KIND" --env "NAME=$NAME" --env "IMAGE=$IMAGE" \
  "$@" --detach -- sh /opt/a2a/deploy/ctl.sh
echo "controller started: $APP ctl-$NAME ($KIND)"
