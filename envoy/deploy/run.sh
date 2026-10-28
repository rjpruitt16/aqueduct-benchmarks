#!/bin/sh
# One measured run on Fly.io: creates a private app with the backend and its
# front door in one region, load clients in several regions, waits for every
# client's report, copies the results out, and destroys the app.
#
# Usage: deploy/run.sh <envoy|envoy-max|aquifer|both|envoy-idem|envoy-queue> <steady|changing|scaleout|chains|wild|survival|smoke|scaleout-smoke|chains-smoke|wild-smoke|survival-smoke> <run-name>
#
# Front doors: envoy, aquifer, or both (Envoy at the edge, Aquifer behind it).
# For the wild regime, two more keep Envoy and add what a team would build
# itself, with Redis (see DESIGN-diy.md): envoy-idem (idempotency keys in the
# bottom services) and envoy-queue (idempotency plus a per-tenant queue that
# cmd/dispatcher runs through Envoy).
# envoy-max is Envoy with its rate limits sized for the capacity after
# scaling in the survival regime (see DESIGN-survival.md) instead of the
# starting capacity.
#
# Environment:
#   FLY_ORG        Fly organization (required)
#   IMAGE          ghcr.io/rjpruitt16/aqueduct-benchmarks-envoy:<commit> (required)
#   ENVOY_IMAGE    default envoyproxy/envoy:v1.39.3
#   REGION         region of the backend and front door (default ord)
#   KEEP=1         leave the app running after the run, for debugging
#   AQUIFER_REF    the Aquifer release or commit built into IMAGE, recorded
#                  in run.txt
#   ADVERTISE      the backends' header mode, live (default; split by the
#                  caller's active queues), total (Aquifer 0.16 and later;
#                  the survival default) or none
#   SCALE_DELAY    survival: minutes from the start of the overload until
#                  the new capacity arrives (default 5)
#   NO_KEY=1       survival: callers send a new idempotency key on every
#                  attempt
#   HOLD=1         survival, aquifer: callers hold the /proxy stream and
#                  reconnect at once with the same key (Aquifer's own client)
#   POLL=1         survival, aquifer: callers submit to /jobs once and poll
#                  GET /jobs/{id} at Aquifer's Retry-After
#   CLIENT_MEMORY  survival: load client memory in MB (default 2048)
#   UNBOUNDED=1    survival: callers retry until their job is done, with no
#                  patience (PATIENCE=0), instead of giving up after 5 minutes
#
# The survival regime asks how long a front door can hold out until new
# capacity arrives. Independent jobs, the tenants below, and callers that
# all retry the same way whatever the front door: up to 10 s per attempt,
# exponential backoff with full jitter up to 60 s, honoring Retry-After,
# and giving up 5 minutes after the job was created (small-4 is
# aggressive instead: every 100 ms, ignoring Retry-After). Envoy callers
# send to /work, Aquifer callers to /proxy with the job's ID as the
# idempotency key. Load: 0.5x for 3 minutes, 3x for 15, then 0.5x for 12;
# the backend goes from 8 to 32 workers SCALE_DELAY minutes into the 3x.
# It advertises its totals, with X-Aqueduct-Max-Backlog at 30 minutes of
# its capacity.
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
# then 2x for 10, with capacity doubling 20 minutes in. Each tenant runs on
# its own client machine, so one tenant's retries can't slow another's.
# Tenants, by region and retry style (1x = 40 jobs/s):
#   big          16  REGION  polite
#   near-polite   4  REGION  polite
#   near-aggr     4  REGION  aggressive
#   far-polite    4  lhr     polite
#   far-aggr      4  lhr     aggressive
#   vfar-polite   4  syd     polite
#   vfar-aggr     4  syd     aggressive
#
# The wild regime puts four microservices between the clients and the front
# door (cmd/service): api -> orders -> inventory + payments -> front door ->
# backend, each retrying what it calls 3 times, 50 and 100 ms apart, with
# per-try timeouts of 10, 8 and 5 s. A client request needs two backend
# jobs. Clients time out after 15 s and retry until they succeed, in four
# styles (polite, nojitter, aggressive, stack: see cmd/client). Bursts of 4x
# come on top of the phases, and the backend is down for 60 s at the peak.
# 15 tenants in requests/s at 1x (1x = 21 requests/s = 42 jobs/s), each with
# its own workers, two client machines per region (the large tenant alone):
#   ord  L1 3 polite x40; s1 polite, s2 nojitter, s3 aggressive, s4 stack, 1 each x10
#   lhr  L2 3 nojitter x40; s5-s8 as in ord
#   syd  L3 3 stack x40; s9-s12 as in ord
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

MODE=${1:?usage: run.sh <envoy|envoy-max|aquifer|both|envoy-idem|envoy-queue> <steady|changing|scaleout|chains|wild|survival|smoke|scaleout-smoke|chains-smoke|wild-smoke|survival-smoke> <run-name>}
REGIME=${2:?usage: run.sh <envoy|envoy-max|aquifer|both|envoy-idem|envoy-queue> <steady|changing|scaleout|chains|wild|survival|smoke|scaleout-smoke|chains-smoke|wild-smoke|survival-smoke> <run-name>}
NAME=${3:?usage: run.sh <envoy|envoy-max|aquifer|both|envoy-idem|envoy-queue> <steady|changing|scaleout|chains|wild|survival|smoke|scaleout-smoke|chains-smoke|wild-smoke|survival-smoke> <run-name>}
: "${FLY_ORG:?set FLY_ORG}"
: "${IMAGE:?set IMAGE}"
ENVOY_IMAGE=${ENVOY_IMAGE:-envoyproxy/envoy:v1.39.3}
REGION=${REGION:-ord}
APP=aqb-envoy-$NAME
OUT=results/$NAME
HERE=$(cd "$(dirname "$0")" && pwd)
case "$MODE" in envoy | envoy-max | aquifer | both | envoy-idem | envoy-queue) ;; *) echo "mode must be envoy, envoy-max, aquifer, both, envoy-idem or envoy-queue" >&2; exit 2 ;; esac
EXTRA_BACKENDS=""
ONLINE_AFTER=15m
GLOBAL_RPS=80
WORKLOAD=jobs
BURSTS=""
OUTAGE=""
DEFAULT_ADVERTISE=live
BACKLOG_SECONDS=60
SCALE_DELAY=${SCALE_DELAY:-5}
PATIENCE=""
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
  wild)
    PHASES=warm:5m:0.5,normal:10m:1,peak:10m:3,after:10m:1.5,cool:5m:0.5
    CAPACITY_SCHEDULE=25m:16
    WORKLOAD=wild
    # Fixed, so every run gets the same traffic.
    BURSTS=6m:30s,8m40s:15s,11m20s:60s,13m10s:20s,26m30s:45s,29m:10s,32m15s:30s
    OUTAGE=20m:60s
    ;;
  wild-smoke)
    PHASES=warm:1m:0.5,peak:3m:3,cool:1m:0.5
    CAPACITY_SCHEDULE=3m30s:16
    WORKLOAD=wild
    BURSTS=40s:10s
    OUTAGE=2m:30s
    ;;
  survival)
    PHASES=warm:3m:0.5,overload:15m:3,cool:12m:0.5
    CAPACITY_SCHEDULE=$((3 + SCALE_DELAY))m:32
    PATIENCE=5m
    # Callers that retry until their job is done, as callers in the wild do.
    [ "${UNBOUNDED:-}" = 1 ] && PATIENCE=0
    ;;
  survival-smoke)
    PHASES=warm:1m:0.5,overload:4m:3,cool:3m:0.5
    SCALE_DELAY=1
    CAPACITY_SCHEDULE=2m:32
    PATIENCE=2m
    ;;
  *) echo "regime must be steady, changing, scaleout, chains, wild, survival, smoke, scaleout-smoke, chains-smoke, wild-smoke or survival-smoke" >&2; exit 2 ;;
esac
case "$REGIME" in survival*)
  DEFAULT_ADVERTISE=total
  BACKLOG_SECONDS=1800
  # Both front doors may use the capacity after scaling: Envoy's own limits
  # decide how much it lets through (envoy-max below), Aquifer follows the
  # backend's headers within this ceiling.
  GLOBAL_RPS=320
  # Callers hold one stream per job waiting in Aquifer's queue; 1 GB ran out
  # for the big tenant once the backlog grew (surv-aquifer-d3-1, first try).
  CLIENT_MEMORY=${CLIENT_MEMORY:-2048}
  ;;
esac
ADV=${ADVERTISE:-$DEFAULT_ADVERTISE}
[ "$ADV" = none ] && ADV=""
for tool in fly jq base64; do
  command -v "$tool" >/dev/null || { echo "run.sh needs $tool" >&2; exit 2; }
done
mkdir -p "$OUT"

# Client machines, region:tenants each, and the tenants each one plays.
CLIENTS="$REGION:big:20 iad:medium:10,small-1:2.5 dfw:small-2:2.5,small-3:2.5 lhr:small-4:2.5"
if [ "$WORKLOAD" = chains ]; then
  CLIENTS="$REGION:big:16 $REGION:near-polite:4 $REGION:near-aggr:4:aggressive lhr:far-polite:4 lhr:far-aggr:4:aggressive syd:vfar-polite:4 syd:vfar-aggr:4:aggressive"
fi
if [ "$WORKLOAD" = wild ]; then
  CLIENTS="$REGION:L1:3:polite:40 $REGION:s1:1:polite:10,s2:1:nojitter:10,s3:1:aggressive:10,s4:1:stack:10
lhr:L2:3:nojitter:40 lhr:s5:1:polite:10,s6:1:nojitter:10,s7:1:aggressive:10,s8:1:stack:10
syd:L3:3:stack:40 syd:s9:1:polite:10,s10:1:nojitter:10,s11:1:aggressive:10,s12:1:stack:10"
fi
case "$REGIME" in survival*)
  CLIENTS="$REGION:big:20 iad:medium:10,small-1:2.5 dfw:small-2:2.5,small-3:2.5 lhr:small-4:2.5:aggressive"
  ;;
esac
TENANT_RPS=20 # per-tenant limit in Envoy: half the starting capacity
ENVOY_RPS=$GLOBAL_RPS
case "$REGIME:$MODE" in
  survival*:envoy) ENVOY_RPS=80 ;; # twice the starting capacity, as in every series
  # The same proportions for the capacity after scaling (32 workers, 160/s).
  survival*:envoy-max) TENANT_RPS=80 ENVOY_RPS=320 ;;
esac
# A client machine's name: its region and first tenant, e.g. ord-big.
cname() {
  t=${1#*:}
  echo "${1%%:*}-${t%%:*}"
}

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
  -e ADVERTISE="$ADV" -e BACKLOG_SECONDS="$BACKLOG_SECONDS" -e CAPACITY_SCHEDULE="$CAPACITY_SCHEDULE" >/dev/null
BACKEND=$(host backend)
BACKENDS=$BACKEND
# Machines that join mid-run: running from the start, but off the network
# until ONLINE_AFTER, so the moment they come online is exact.
for spec in $EXTRA_BACKENDS; do
  run_machine "${spec%%:*}" "$IMAGE" -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
    --restart on-failure --detach -e ROLE=backend -e WORKERS="${spec#*:}" -e SERVICE_MS=200 -e QUEUE_MAX=64 \
    -e ADVERTISE="$ADV" -e ONLINE_AFTER="$ONLINE_AFTER" -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
  BACKENDS="$BACKENDS $(host "${spec%%:*}")"
done

# The shared store for the do-it-yourself setups: Redis with append-only
# persistence, as a team would run it for a queue.
REDIS_URL=""
case "$MODE" in envoy-idem | envoy-queue)
  [ "$WORKLOAD" = wild ] || fail "$MODE is for the wild regime"
  echo "== redis"
  rcfg=$(mktemp)
  printf 'bind * -::*\nprotected-mode no\nappendonly yes\nappendfsync everysec\nsave ""\ndir /data\n' > "$rcfg"
  run_machine redis redis:7.4-alpine -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
    --restart on-failure --detach --file-local /etc/redis.conf="$rcfg" redis-server /etc/redis.conf >/dev/null
  rm -f "$rcfg"
  REDIS_URL="redis://$(host redis):6379"
  ;;
esac

# The microservices, bottom first so each knows the hosts it calls. The
# bottom ones learn the front door from the run's settings.
API=""
if [ "$WORKLOAD" = wild ]; then
  echo "== services"
  svc() {
    run_machine "svc-$1" "$IMAGE" -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
      --restart on-failure --detach -e ROLE=service -e NAME="$1" -e TIMEOUT="$2" -e DOWNSTREAM="$3" \
      -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
  }
  svc inventory 5s ""
  svc payments 5s ""
  svc orders 8s "http://$(host svc-inventory):8080,http://$(host svc-payments):8080"
  svc api 10s "http://$(host svc-orders):8080"
  API="http://$(host svc-api):8080"
fi

# Clients start now and wait for the run's shared settings, which the backend
# serves once every machine is up: updating a machine in a far region can
# take minutes, starting one takes seconds.
echo "== clients"
for spec in $CLIENTS; do
  region=${spec%%:*}
  run_machine "client-$(cname "$spec")" "$IMAGE" -r "$region" --vm-size shared-cpu-2x --vm-memory "${CLIENT_MEMORY:-1024}" \
    --restart no --detach -e ROLE=client -e NAME="$(cname "$spec")" -e TENANTS="${spec#*:}" \
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
if [ "$MODE" != aquifer ] && [ "$MODE" != both ]; then
  echo "== envoy"
  # shellcheck disable=SC2086
  envoy_config envoy.yaml.tmpl "$TENANT_RPS" "$ENVOY_RPS" $BACKENDS
  anonymize "$cfg" > "$OUT/envoy.yaml"
  run_machine front "$ENVOY_IMAGE" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/envoy/envoy.yaml="$cfg" >/dev/null
  FRONT="http://$(host front):8080"
  if [ "$MODE" = envoy-queue ]; then
    echo "== dispatcher"
    run_machine dispatcher "$IMAGE" -r "$REGION" --vm-size shared-cpu-2x --vm-memory 1024 \
      --restart on-failure --detach -e ROLE=dispatcher -e CONFIG_URL="http://$BACKEND:8080/config" >/dev/null
  fi
else
  echo "== aquifer"
  {
    if [ -n "$EXTRA_BACKENDS" ]; then
      # Jobs go to a pool, whose queues take their ceiling from the defaults.
      printf 'defaults:\n  rps: %s\n  max_concurrent: 32\nupstreams:\n' "$GLOBAL_RPS"
    elif [ -z "$ADV" ]; then
      # No headers to follow: limits a team would set for the starting
      # capacity, as Envoy's are (80/s in total; 2 in flight per tenant
      # queue, 12 for the six tenants against 8 workers).
      printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n'
      printf '  "%s:8080":\n    rps: 80\n    max_concurrent: 2\n' "$BACKEND"
    else
      printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n'
      printf '  "%s:8080":\n    rps: %s\n    max_concurrent: 32\n' "$BACKEND" "$GLOBAL_RPS"
    fi
    # Webhooks to the clients (the services wait on /proxy instead).
    if [ "$WORKLOAD" != wild ]; then
      for spec in $CLIENTS; do
        printf '  "%s:8081":\n    rps: 1000\n    max_concurrent: 64\n' "$(host "client-$(cname "$spec")")"
      done
    fi
  } > "$cfg"
  anonymize "$cfg" > "$OUT/aquifer.yaml"
  run_machine aquifer "$IMAGE" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/aquifer.yaml="$cfg" \
    -e ROLE=aquifer -e AQUIFER_ADAPTER=http -e PORT=8080 -e DB_PATH=/tmp/aquifer -e CONFIG_PATH=/etc/aquifer.yaml \
    -e AQUIFER_SHARED_DESTINATION_LIMITS=true -e AQUIFER_READY_ESTIMATES=true >/dev/null
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
  --arg api "$API" --arg bursts "$BURSTS" --arg outage "$OUTAGE" --arg redis "$REDIS_URL" \
  --arg patience "$PATIENCE" --arg nokey "${NO_KEY:-}" --arg hold "${HOLD:-}" --arg call "$([ "${POLL:-}" = 1 ] && echo poll || echo proxy)" \
  '{MODE: $mode, TARGET: $target, BACKEND_URL: $backend, PHASES: $phases, START_AT: $start, REPORT_URL: $report}
   + (if $pool == "" then {} else {POOL: $pool, REGISTER_URL: $register} end)
   + (if $workload == "chains" then {WORKLOAD: "chains", MAX_ATTEMPTS: "0", BACKOFF_MAX: "60s", DEADLINE_SHARE: "0", DRAIN: "60m"} else {} end)
   + (if $workload == "wild" then {WORKLOAD: "wild", TARGET: $api, FRONT_URL: $target, BURSTS: $bursts, OUTAGE: $outage,
        MAX_ATTEMPTS: "0", BACKOFF_MAX: "60s", DEADLINE_SHARE: "0", TIMEOUT: "15s", DRAIN: "60m"} else {} end)
   + (if $redis == "" then {} else {REDIS_URL: $redis} end)
   + (if $patience == "" then {} else {PATIENCE: $patience, CALL: $call, NO_KEY: $nokey, HOLD: $hold, JOB_LOG: "1",
        MAX_ATTEMPTS: "0", BACKOFF_MAX: "60s", DEADLINE_SHARE: "0", TIMEOUT: "10s", DRAIN: (if $patience == "0" then "60m" else "30m" end)} end)')
retry on backend "wget -qO- --header Content-Type:application/json --post-data '$config' http://localhost:8080/config" >/dev/null ||
  fail "could not publish the run's settings"
echo "== load begins at $(date -u -d "@$START_AT" +%TZ 2>/dev/null || date -u -r "$START_AT" +%TZ)"

echo "== waiting for the clients' reports"
i=0
until [ "$(on backend "ls /out/clients" | grep -v '^svc-' | grep -c json)" -ge "$want" ]; do
  i=$((i + 1))
  [ "$i" -gt 240 ] && fail "clients did not report within 2 hours"
  sleep 30
done
# The services (and the dispatcher) report every 10 s; let them send their
# last counts.
[ "$WORKLOAD" = wild ] && sleep 20
# Work Aquifer still holds after its callers gave up runs on; give it time
# to finish before the backend's counts are copied.
[ -n "$PATIENCE" ] && [ "$MODE" = aquifer ] && sleep 120

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
  echo "bursts=$BURSTS"
  echo "outage=$OUTAGE"
  echo "capacity_schedule=$CAPACITY_SCHEDULE"
  echo "extra_backends=$EXTRA_BACKENDS"
  echo "global_rps=$GLOBAL_RPS"
  echo "advertise=${ADV:-none}"
  echo "backlog_seconds=$BACKLOG_SECONDS"
  echo "client_memory_mb=${CLIENT_MEMORY:-1024}"
  if [ -n "$PATIENCE" ]; then
    echo "scale_delay_min=$SCALE_DELAY"
    echo "patience=$PATIENCE"
    echo "no_key=${NO_KEY:-0}"
    echo "hold=${HOLD:-0}"
    echo "poll=${POLL:-0}"
    echo "envoy_rps=$ENVOY_RPS"
    echo "tenant_rps=$TENANT_RPS"
  fi
  [ -n "${AQUIFER_REF:-}" ] && echo "aquifer_ref=$AQUIFER_REF"
} > "$OUT/run.txt"
echo "== done: $OUT"
