#!/bin/sh
# One measured Envoy-or-Aquifer run on Fly.io: creates a private app with the
# backend and its front door (Envoy or Aquifer) in one region, load clients
# in several regions, waits for every client's report, copies the results
# out, and destroys the app.
#
# Usage: deploy/run.sh <envoy|aquifer> <steady|changing|smoke> <run-name>
#
# Environment:
#   FLY_ORG        Fly organization (required)
#   IMAGE          ghcr.io/rjpruitt16/aqueduct-benchmarks-envoy:<commit> (required)
#   ENVOY_IMAGE    default envoyproxy/envoy:v1.39.3
#   REGION         region of the backend and front door (default ord)
#   KEEP=1         leave the app running after the run, for debugging
#
# Capacity: the backend starts with 8 workers at 200 ms (40 jobs/s). In the
# changing regime it doubles to 16 workers 15 minutes in, while overloaded,
# the way an autoscaler catches up late. Both front doors get the same
# ceilings: 80 requests/s and 32-64 in flight.
#
# Tenants (jobs/s at 1x load, 1x = 40 jobs/s = the starting capacity):
#   big      20    in REGION, next to the backend
#   medium   10    iad
#   small-1  2.5   iad
#   small-2  2.5   dfw
#   small-3  2.5   dfw
#   small-4  2.5   lhr
#
# Machines: backend shared-cpu-2x; the front door performance-1x (the same
# for Envoy and Aquifer); one shared-cpu-2x client per region.
set -eu

MODE=${1:?usage: run.sh <envoy|aquifer> <steady|changing> <run-name>}
REGIME=${2:?usage: run.sh <envoy|aquifer> <steady|changing> <run-name>}
NAME=${3:?usage: run.sh <envoy|aquifer> <steady|changing> <run-name>}
: "${FLY_ORG:?set FLY_ORG}"
: "${IMAGE:?set IMAGE}"
ENVOY_IMAGE=${ENVOY_IMAGE:-envoyproxy/envoy:v1.39.3}
REGION=${REGION:-ord}
APP=aqb-envoy-$NAME
OUT=results/$NAME
HERE=$(cd "$(dirname "$0")" && pwd)
case "$MODE" in envoy | aquifer) ;; *) echo "mode must be envoy or aquifer" >&2; exit 2 ;; esac
case "$REGIME" in
  smoke)
    PHASES=steady:2m:0.5
    CAPACITY_SCHEDULE=
    ;;
  steady)
    PHASES=steady:20m:0.5
    CAPACITY_SCHEDULE=
    ;;
  changing)
    PHASES=steady:5m:0.5,spike:5m:3,sustained:10m:1.5,recovery:10m:0.5
    CAPACITY_SCHEDULE=15m:16
    ;;
  *) echo "regime must be steady or changing" >&2; exit 2 ;;
esac
for tool in fly jq base64; do
  command -v "$tool" >/dev/null || { echo "run.sh needs $tool" >&2; exit 2; }
done
mkdir -p "$OUT"

# Client regions and the tenants each one plays.
CLIENTS="$REGION:big:20 iad:medium:10,small-1:2.5 dfw:small-2:2.5,small-3:2.5 lhr:small-4:2.5"
TENANT_RPS=20 # per-tenant limit in Envoy: half the starting capacity
GLOBAL_RPS=80

fail() {
  echo "$1" >&2
  exit 1
}

cleanup() {
  status=$?
  fly logs -a "$APP" --no-tail > "$OUT/fly.log" 2>/dev/null || true
  if [ "${KEEP:-}" = 1 ]; then
    return
  fi
  if [ "$status" != 0 ]; then
    for id in $(fly machine list -a "$APP" --json 2>/dev/null | jq -r '.[] | select(.state == "started") | .id'); do
      fly machine stop "$id" -a "$APP" >/dev/null 2>&1 || true
    done
    echo "run.sh failed (status $status); stopped $APP's machines and kept it for inspection; destroy it with: fly apps destroy $APP -y" >&2
  else
    fly apps destroy "$APP" -y >/dev/null 2>&1 || true
  fi
}

# Retries a Fly call that is safe to repeat; the API times out now and then.
retry() {
  for try in 1 2 3 4; do
    "$@" && return 0
    sleep 10
  done
  return 1
}

machine_id() { fly machine list -a "$APP" --json | jq -r --arg n "$1" '.[] | select(.name == $n) | .id'; }
host() { echo "$(machine_id "$1").vm.$APP.internal"; }
# Runs a command on a machine and prints its output.
on() {
  m=$1
  shift
  fly ssh console -a "$APP" --machine "$(machine_id "$m")" -C "$*" 2>/dev/null
}

echo "== creating $APP in $FLY_ORG/$REGION ($MODE, $REGIME)"
fly apps create "$APP" -o "$FLY_ORG" >/dev/null
trap cleanup EXIT

echo "== backend"
fly machine run "$IMAGE" -a "$APP" -n backend -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
  --restart on-failure --detach -e ROLE=backend -e WORKERS=8 -e SERVICE_MS=200 -e QUEUE_MAX=64 \
  -e ADVERTISE=1 -e CAPACITY_SCHEDULE="$CAPACITY_SCHEDULE" >/dev/null
BACKEND=$(host backend)

# Clients start now and wait for the run's shared settings, which the backend
# serves once every machine is up: updating a machine in a far region can
# take minutes, starting one takes seconds.
echo "== clients"
for spec in $CLIENTS; do
  region=${spec%%:*}
  fly machine run "$IMAGE" -a "$APP" -n "client-$region" -r "$region" --vm-size shared-cpu-2x --vm-memory 1024 \
    --restart no --detach -e ROLE=client -e NAME="$region" -e TENANTS="${spec#*:}" \
    -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
done

cfg=$(mktemp)
if [ "$MODE" = envoy ]; then
  echo "== envoy"
  descriptors=""
  for spec in $CLIENTS; do
    for t in $(echo "${spec#*:}" | tr ',' ' '); do
      descriptors="$descriptors                                - entries: [{ key: tenant, value: ${t%%:*} }]
                                  token_bucket: { max_tokens: $((TENANT_RPS * 2)), tokens_per_fill: $TENANT_RPS, fill_interval: 1s }
"
    done
  done
  printf '%s' "$descriptors" > "$cfg.d"
  sed -e "s/__BACKEND_HOST__/$BACKEND/" -e "s/__GLOBAL_RPS__/$GLOBAL_RPS/g" \
    -e "/__DESCRIPTORS__/{r $cfg.d
d;}" "$HERE/envoy.yaml.tmpl" > "$cfg"
  rm -f "$cfg.d"
  cp "$cfg" "$OUT/envoy.yaml"
  fly machine run "$ENVOY_IMAGE" -a "$APP" -n front -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/envoy/envoy.yaml="$cfg" >/dev/null
  FRONT="http://$(host front):8080"
else
  echo "== aquifer"
  {
    printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n'
    printf '  "%s:8080":\n    rps: %s\n    max_concurrent: 32\n' "$BACKEND" "$GLOBAL_RPS"
    for spec in $CLIENTS; do
      printf '  "%s:8081":\n    rps: 1000\n    max_concurrent: 64\n' "$(host "client-${spec%%:*}")"
    done
  } > "$cfg"
  sed "s/[0-9a-f]*\.vm\.$APP\.internal/<machine>/" "$cfg" > "$OUT/aquifer.yaml"
  fly machine run "$IMAGE" -a "$APP" -n front -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/aquifer.yaml="$cfg" \
    -e ROLE=aquifer -e AQUIFER_ADAPTER=http -e PORT=8080 -e DB_PATH=/tmp/aquifer -e CONFIG_PATH=/etc/aquifer.yaml >/dev/null
  FRONT="http://$(host front):8080"
fi
rm -f "$cfg"

# Wait for the front door to answer before starting the clock.
i=0
until on backend "wget -qO- -T 3 $FRONT/health" >/dev/null || on backend "wget -qO- -T 3 ${FRONT%:8080}:9901/ready" >/dev/null; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && fail "front door did not come up"
  sleep 5
done

want=$(echo "$CLIENTS" | wc -w | tr -d ' ')
i=0
until [ "$(fly machine list -a "$APP" --json | jq '[.[] | select((.name | startswith("client-")) and .state == "started")] | length')" -ge "$want" ]; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && fail "not every client started within 10 minutes"
  sleep 10
done

START_AT=$(($(date +%s) + 60))
config=$(jq -nc --arg mode "$MODE" --arg target "$FRONT" --arg backend "http://$BACKEND:8080/work" \
  --arg phases "$PHASES" --arg start "$START_AT" --arg report "http://$BACKEND:8080/report" \
  '{MODE: $mode, TARGET: $target, BACKEND_URL: $backend, PHASES: $phases, START_AT: $start, REPORT_URL: $report}')
retry on backend "wget -qO- --header Content-Type:application/json --post-data '$config' http://localhost:8080/config" >/dev/null ||
  fail "could not publish the run's settings"
echo "== load begins at $(date -u -d "@$START_AT" +%TZ 2>/dev/null || date -u -r "$START_AT" +%TZ)"

echo "== waiting for the clients' reports"
i=0
until [ "$(on backend "ls /out/clients" | grep -c json)" -ge "$want" ]; do
  i=$((i + 1))
  [ "$i" -gt 180 ] && fail "clients did not report within 90 minutes"
  sleep 30
done

echo "== copying results to $OUT"
if [ "$MODE" = envoy ]; then
  on backend "wget -qO- ${FRONT%:8080}:9901/stats?format=json" > "$OUT/envoy-stats.json" || true
else
  on backend "wget -qO- $FRONT/metrics" | grep '^aquifer_' | sed 's/[0-9a-f]*\.vm\.[a-z0-9-]*\.internal/<machine>/g' > "$OUT/aquifer-metrics.txt" || true
fi
# fly ssh console sometimes cuts output short; retry until the archive is whole.
copied=""
for try in 1 2 3 4 5; do
  on backend "sh -c 'tar czf - -C /out . | base64'" | tr -d '\r' | base64 -d > "$OUT/.bundle.tgz" 2>/dev/null || true
  if gzip -t "$OUT/.bundle.tgz" 2>/dev/null && tar xzf "$OUT/.bundle.tgz" -C "$OUT"; then
    copied=1
    break
  fi
  sleep 10
done
rm -f "$OUT/.bundle.tgz"
[ -n "$copied" ] || fail "could not copy the backend's results"
fly machine list -a "$APP" --json | jq '[.[] | {id, name, region, image: .config.image, guest: .config.guest}]' > "$OUT/machines.json"
{
  echo "mode=$MODE"
  echo "regime=$REGIME"
  echo "start_at=$START_AT"
  echo "image=$IMAGE"
  [ "$MODE" = envoy ] && echo "envoy_image=$ENVOY_IMAGE"
  echo "region=$REGION"
  echo "clients=$CLIENTS"
  echo "phases=$PHASES"
  echo "capacity_schedule=$CAPACITY_SCHEDULE"
} > "$OUT/run.txt"
echo "== done: $OUT"
