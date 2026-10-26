#!/bin/sh
# One measured run on Fly.io: creates a private app with the backend and its
# front door in one region, load clients in several regions, waits for every
# client's report, copies the results out, and destroys the app.
#
# Usage: deploy/run.sh <envoy|aquifer|both> <steady|changing|scaleout|chains|smoke|scaleout-smoke|chains-smoke> <run-name>
#
# Front doors: envoy, aquifer, or both (Envoy at the edge, Aquifer behind it).
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
# the way an autoscaler catches up late. In the scaleout regime two more
# machines come online 15 minutes in instead, one as big (8 workers) and one
# half the size (4): Envoy finds them through its health checks, Aquifer when
# they register in its pool. Every backend sends Aqueduct's live headers
# (Envoy ignores them). Both front doors get the same ceilings: 80 requests/s
# (100 in scaleout, the capacity after it) and 32-64 in flight.
#
# The chains regime is a different workload: tenants work in lanes, each
# running chains of four dependent jobs, and retry every refused step until
# it succeeds (polite tenants with exponential backoff up to 60 s, aggressive
# ones every 100 ms). The overload is deeper and longer: 3x for 15 minutes,
# then 2x for 10, with capacity doubling 20 minutes in. Its tenants, by
# region and retry style (1x = 40 jobs/s):
#   big          20  REGION  polite
#   near-polite   5  REGION  polite
#   near-aggr     5  REGION  aggressive
#   far-polite    5  lhr     polite
#   far-aggr      5  lhr     aggressive
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

MODE=${1:?usage: run.sh <envoy|aquifer|both> <steady|changing|scaleout|chains|smoke|scaleout-smoke|chains-smoke> <run-name>}
REGIME=${2:?usage: run.sh <envoy|aquifer|both> <steady|changing|scaleout|chains|smoke|scaleout-smoke|chains-smoke> <run-name>}
NAME=${3:?usage: run.sh <envoy|aquifer|both> <steady|changing|scaleout|chains|smoke|scaleout-smoke|chains-smoke> <run-name>}
: "${FLY_ORG:?set FLY_ORG}"
: "${IMAGE:?set IMAGE}"
ENVOY_IMAGE=${ENVOY_IMAGE:-envoyproxy/envoy:v1.39.3}
REGION=${REGION:-ord}
APP=aqb-envoy-$NAME
OUT=results/$NAME
HERE=$(cd "$(dirname "$0")" && pwd)
case "$MODE" in envoy | aquifer | both) ;; *) echo "mode must be envoy, aquifer or both" >&2; exit 2 ;; esac
EXTRA_BACKENDS=""
ONLINE_AFTER=15m
GLOBAL_RPS=80
WORKLOAD=jobs
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
  scaleout)
    PHASES=steady:5m:0.5,spike:5m:3,sustained:10m:1.5,recovery:10m:0.5
    CAPACITY_SCHEDULE=
    EXTRA_BACKENDS="backend-2:8 backend-3:4"
    GLOBAL_RPS=100
    ;;
  scaleout-smoke)
    PHASES=steady:2m:0.5,spike:3m:3
    CAPACITY_SCHEDULE=
    EXTRA_BACKENDS="backend-2:8 backend-3:4"
    ONLINE_AFTER=3m
    GLOBAL_RPS=100
    ;;
  chains)
    PHASES=steady:5m:0.5,overload:15m:3,sustained:10m:2,recovery:10m:0.5
    CAPACITY_SCHEDULE=20m:16
    WORKLOAD=chains
    ;;
  chains-smoke)
    PHASES=steady:2m:0.5,overload:4m:3,recovery:2m:0.5
    CAPACITY_SCHEDULE=4m:16
    WORKLOAD=chains
    ;;
  *) echo "regime must be steady, changing, scaleout, chains, smoke, scaleout-smoke or chains-smoke" >&2; exit 2 ;;
esac
for tool in fly jq base64; do
  command -v "$tool" >/dev/null || { echo "run.sh needs $tool" >&2; exit 2; }
done
mkdir -p "$OUT"

# Client regions and the tenants each one plays.
CLIENTS="$REGION:big:20 iad:medium:10,small-1:2.5 dfw:small-2:2.5,small-3:2.5 lhr:small-4:2.5"
if [ "$WORKLOAD" = chains ]; then
  CLIENTS="$REGION:big:20,near-polite:5,near-aggr:5:aggressive lhr:far-polite:5,far-aggr:5:aggressive"
fi
TENANT_RPS=20 # per-tenant limit in Envoy: half the starting capacity

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

# Creates a machine, retrying when the API times out. A timed-out call may
# still have created it, so check by name before trying again.
run_machine() {
  name=$1
  shift
  for try in 1 2 3 4; do
    [ -n "$(machine_id "$name")" ] && return 0
    fly machine run "$@" -a "$APP" -n "$name" >/dev/null && return 0
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
run_machine backend "$IMAGE" -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
  --restart on-failure --detach -e ROLE=backend -e WORKERS=8 -e SERVICE_MS=200 -e QUEUE_MAX=64 \
  -e ADVERTISE=live -e CAPACITY_SCHEDULE="$CAPACITY_SCHEDULE" >/dev/null
BACKEND=$(host backend)
BACKENDS=$BACKEND
# Machines that join mid-run: running from the start, but off the network
# until ONLINE_AFTER, so the moment they come online is exact.
for spec in $EXTRA_BACKENDS; do
  run_machine "${spec%%:*}" "$IMAGE" -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
    --restart on-failure --detach -e ROLE=backend -e WORKERS="${spec#*:}" -e SERVICE_MS=200 -e QUEUE_MAX=64 \
    -e ADVERTISE=live -e ONLINE_AFTER="$ONLINE_AFTER" -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
  BACKENDS="$BACKENDS $(host "${spec%%:*}")"
done

# Clients start now and wait for the run's shared settings, which the backend
# serves once every machine is up: updating a machine in a far region can
# take minutes, starting one takes seconds.
echo "== clients"
for spec in $CLIENTS; do
  region=${spec%%:*}
  run_machine "client-$region" "$IMAGE" -r "$region" --vm-size shared-cpu-2x --vm-memory 1024 \
    --restart no --detach -e ROLE=client -e NAME="$region" -e TENANTS="${spec#*:}" \
    -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
done

# One rate-limit descriptor per tenant at the given rate, for an Envoy front.
descriptors() {
  for spec in $CLIENTS; do
    for t in $(echo "${spec#*:}" | tr ',' ' '); do
      printf '                                - entries: [{ key: tenant, value: %s }]\n' "${t%%:*}"
      printf '                                  token_bucket: { max_tokens: %s, tokens_per_fill: %s, fill_interval: 1s }\n' $(($1 * 2)) "$1"
    done
  done
}
# lb_endpoints for the given hosts, port 8080.
endpoints() {
  for h in "$@"; do
    printf '              - endpoint: { address: { socket_address: { address: %s, port_value: 8080 } } }\n' "$h"
  done
}
# envoy_config <template> <tenant rps> <global rps> <hosts...> fills in a
# template into $cfg.
envoy_config() {
  tmpl=$1 global=$3
  descriptors "$2" > "$cfg.d"
  shift 3
  endpoints "$@" > "$cfg.e"
  sed -e "s/__GLOBAL_RPS__/$global/g" -e "/__DESCRIPTORS__/{r $cfg.d
d;}" -e "/__ENDPOINTS__/{r $cfg.e
d;}" "$HERE/$tmpl" > "$cfg"
  rm -f "$cfg.d" "$cfg.e"
}
anonymize() { sed "s/[0-9a-f]*\.vm\.$APP\.internal/<machine>/" "$@"; }

cfg=$(mktemp)
POOL=""
REGISTER_URL=""
AQUIFER=""
if [ "$MODE" = envoy ]; then
  echo "== envoy"
  # shellcheck disable=SC2086
  envoy_config envoy.yaml.tmpl "$TENANT_RPS" "$GLOBAL_RPS" $BACKENDS
  anonymize "$cfg" > "$OUT/envoy.yaml"
  run_machine front "$ENVOY_IMAGE" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/envoy/envoy.yaml="$cfg" >/dev/null
  FRONT="http://$(host front):8080"
else
  echo "== aquifer"
  {
    if [ -n "$EXTRA_BACKENDS" ]; then
      # Jobs go to a pool, whose queues take their ceiling from the defaults.
      printf 'defaults:\n  rps: %s\n  max_concurrent: 32\nupstreams:\n' "$GLOBAL_RPS"
    else
      printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n'
      printf '  "%s:8080":\n    rps: %s\n    max_concurrent: 32\n' "$BACKEND" "$GLOBAL_RPS"
    fi
    for spec in $CLIENTS; do
      printf '  "%s:8081":\n    rps: 1000\n    max_concurrent: 64\n' "$(host "client-${spec%%:*}")"
    done
  } > "$cfg"
  anonymize "$cfg" > "$OUT/aquifer.yaml"
  run_machine aquifer "$IMAGE" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/aquifer.yaml="$cfg" \
    -e ROLE=aquifer -e AQUIFER_ADAPTER=http -e PORT=8080 -e DB_PATH=/tmp/aquifer -e CONFIG_PATH=/etc/aquifer.yaml >/dev/null
  AQUIFER="http://$(host aquifer):8080"
  FRONT=$AQUIFER
  if [ -n "$EXTRA_BACKENDS" ]; then
    POOL=backend
    REGISTER_URL="$AQUIFER/pools/backend/members"
  fi
  if [ "$MODE" = both ]; then
    echo "== envoy edge"
    # At the edge the limits only guard against abuse: twice what the
    # backend side allows, per tenant and in total.
    envoy_config envoy-edge.yaml.tmpl $((TENANT_RPS * 2)) $((GLOBAL_RPS * 2)) "$(host aquifer)"
    anonymize "$cfg" > "$OUT/envoy-edge.yaml"
    run_machine front "$ENVOY_IMAGE" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
      --restart on-failure --detach --file-local /etc/envoy/envoy.yaml="$cfg" >/dev/null
    FRONT="http://$(host front):8080"
  fi
fi
rm -f "$cfg"

# Wait for the front door to answer before starting the clock.
i=0
up() { on backend "wget -qO- -T 3 $1/health" >/dev/null || on backend "wget -qO- -T 3 ${1%:8080}:9901/ready" >/dev/null; }
until up "$FRONT" && { [ -z "$AQUIFER" ] || up "$AQUIFER"; }; do
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
  --arg pool "$POOL" --arg register "$REGISTER_URL" --arg workload "$WORKLOAD" \
  '{MODE: $mode, TARGET: $target, BACKEND_URL: $backend, PHASES: $phases, START_AT: $start, REPORT_URL: $report}
   + (if $pool == "" then {} else {POOL: $pool, REGISTER_URL: $register} end)
   + (if $workload == "chains" then {WORKLOAD: "chains", MAX_ATTEMPTS: "0", BACKOFF_MAX: "60s", DEADLINE_SHARE: "0", DRAIN: "60m"} else {} end)')
retry on backend "wget -qO- --header Content-Type:application/json --post-data '$config' http://localhost:8080/config" >/dev/null ||
  fail "could not publish the run's settings"
echo "== load begins at $(date -u -d "@$START_AT" +%TZ 2>/dev/null || date -u -r "$START_AT" +%TZ)"

echo "== waiting for the clients' reports"
i=0
until [ "$(on backend "ls /out/clients" | grep -c json)" -ge "$want" ]; do
  i=$((i + 1))
  [ "$i" -gt 240 ] && fail "clients did not report within 2 hours"
  sleep 30
done

echo "== copying results to $OUT"
if [ "$MODE" != aquifer ]; then
  on backend "wget -qO- ${FRONT%:8080}:9901/stats?format=json" > "$OUT/envoy-stats.json" || true
fi
if [ -n "$AQUIFER" ]; then
  on backend "wget -qO- $AQUIFER/metrics" | grep '^aquifer_' | sed 's/[0-9a-f]*\.vm\.[a-z0-9-]*\.internal/<machine>/g' > "$OUT/aquifer-metrics.txt" || true
  on backend "wget -qO- $AQUIFER/health" | sed 's/[0-9a-f]*\.vm\.[a-z0-9-]*\.internal/<machine>/g' > "$OUT/aquifer-health.json" || true
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
# The machines that joined mid-run keep their own executions and events.
for spec in $EXTRA_BACKENDS; do
  m=${spec%%:*}
  mkdir -p "$OUT/$m"
  copied=""
  for try in 1 2 3 4 5; do
    on "$m" "sh -c 'tar czf - -C /out . | base64'" | tr -d '\r' | base64 -d > "$OUT/.bundle.tgz" 2>/dev/null || true
    if gzip -t "$OUT/.bundle.tgz" 2>/dev/null && tar xzf "$OUT/.bundle.tgz" -C "$OUT/$m"; then
      copied=1
      break
    fi
    sleep 10
  done
  rm -f "$OUT/.bundle.tgz"
  [ -n "$copied" ] || fail "could not copy $m's results"
done
fly machine list -a "$APP" --json | jq '[.[] | {id, name, region, image: .config.image, guest: .config.guest}]' > "$OUT/machines.json"
{
  echo "mode=$MODE"
  echo "regime=$REGIME"
  echo "start_at=$START_AT"
  echo "image=$IMAGE"
  [ "$MODE" != aquifer ] && echo "envoy_image=$ENVOY_IMAGE"
  echo "region=$REGION"
  echo "clients=$CLIENTS"
  echo "phases=$PHASES"
  echo "workload=$WORKLOAD"
  echo "capacity_schedule=$CAPACITY_SCHEDULE"
  echo "extra_backends=$EXTRA_BACKENDS"
  echo "global_rps=$GLOBAL_RPS"
} > "$OUT/run.txt"
echo "== done: $OUT"
