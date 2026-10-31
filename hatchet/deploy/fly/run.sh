#!/bin/sh
# One measured Hatchet benchmark run on Fly.io: creates a private app, runs
# Postgres, Hatchet (engine and API), workers, optionally Aquifer, and the
# driver with the Postgres sampler; copies the results out; destroys the app.
#
# Usage: deploy/fly/run.sh <B|C|D> <run-name>
#
#   B  tuned Hatchet, producers trigger it directly
#   C  tuned Hatchet behind Aquifer
#   D  tuned Hatchet behind Envoy (deploy/fly/envoy.yaml.tmpl)
#   E  tuned Hatchet behind Aquifer behind Envoy: Envoy at the edge, Aquifer holding
#      the work (see gateway-failures/DESIGN.md)
#
# Environment:
#   FLY_ORG         Fly organization (required)
#   REGION          Fly region (default iad)
#   HARNESS_IMAGE   ghcr.io/rjpruitt16/aqueduct-benchmarks-hatchet:<commit> (required)
#   FORK_API_IMAGE  patched Hatchet API image for C
#                   (default ghcr.io/rjpruitt16/hatchet-aquifer-api:v0.110.8-aqueduct-ff494eb)
#   RATE            documents/s at 100% (default 50; set from calibration)
#   PHASES, SCALE   passed to the driver (defaults: the agreed 75-minute run)
#   DRAIN_MAX       how long the driver waits for outstanding work (default 30m)
#   WORKERS         worker machines (default 3)
#   KEEP=1          leave the app running after the run, for debugging
#   AQUIFER_FLUSH_MS  Aquifer's disk flush interval in C (default: Aquifer's 100 ms; 0 = fsync every write)
#   TENANT_MAX_RUNS concurrent documents per tenant in the workflow (default 50)
#   PACING=1        C only: the fork's API sends pacing headers (Aqueduct patch 2),
#                   at most RATE triggers/s, falling above PACING_TARGET_DEPTH
#                   waiting slots (default 1000); Aquifer's own ceiling stays RATE
#   PACING_MAX_RPS  C with PACING=1: the most the fork's API ever advertises (default RATE);
#                   set it, and AQUIFER_RPS, above RATE to let pacing follow a bigger database
#   ENGINE_DB_CONNS, TRIGGER_SLOTS, BUFFER_SIZE, BUFFER_FLUSH
#                   tuned engine settings (see TUNING.md); fixed by calibration
#   MAX_ATTEMPTS    producer trigger attempts before a document is rejected (driver default 6)
#   AQUIFER_MAX_RETRIES  C: Aquifer's retries per job (driver default 20; -1 until it succeeds)
#   TENANT_RPS, GLOBAL_RPS  D: Envoy's per-tenant and total trigger rates (default 15 and RATE)
#   PG_SIZE         Postgres machine: performance-1x, -2x (default) or -4x
#   RESIZE_AT, RESIZE_TO  resize Postgres to RESIZE_TO (e.g. performance-4x) RESIZE_AT seconds
#                   after the driver starts; Postgres restarts on the same volume
#   OUTAGE_AT, OUTAGE_FOR  stop Postgres OUTAGE_AT seconds after the driver starts,
#                   for OUTAGE_FOR seconds
#   IDEMPOTENCY_TTL Hatchet's own workflow idempotency keyed by document, for this long
#                   (off by default); gateway-failures turns it on in every configuration
#   API_VM          Hatchet API machine size, performance-1x or -2x (default: -2x in B, -1x otherwise);
#                   gateway-failures sets it so every configuration has the same budget
#   ENVOY_KILL_AT   D and E: SIGKILL Envoy this many seconds after the driver starts;
#                   Fly's restart policy brings it back
#   ENVOY_DRAIN_AT  D and E: drain Envoy's listeners gracefully at this time, wait
#                   ENVOY_DRAIN_S (default 15), then restart its machine
#   AQUIFER_KILL_AT C and E: SIGKILL Aquifer at this time; it restarts on the same volume
#   API_STOP_AT, API_STOP_FOR  stop Hatchet's API machine at this time, for this many seconds
#   EDGE_TENANT_RPS, EDGE_GLOBAL_RPS  E: Envoy's per-tenant and total limits in front of
#                   Aquifer, set for abuse, not capacity (default 100 and 400)
#
# Machines (all dedicated CPUs, so runs are repeatable):
#   pg       performance-2x, 4 GB, 20 GB volume   Postgres 16 (also Hatchet's message queue)
#   engine   performance-2x, 4 GB                 Hatchet engine
#   api      performance-1x, 2 GB                 Hatchet REST API (the fork's image in C)
#   worker-N performance-1x, 2 GB                 pipeline workers
#   aquifer  performance-1x, 2 GB                 C only; in B the API gets the same budget back
#   envoy    performance-1x, 2 GB                 D only, in front of the API (performance-1x, as in C)
#   driver   performance-2x, 4 GB, 1 GB volume    producers, ledger, Postgres sampler
set -eu

CONFIG=${1:?usage: run.sh <B|C> <run-name>}
NAME=${2:?usage: run.sh <B|C> <run-name>}
: "${FLY_ORG:?set FLY_ORG}"
: "${HARNESS_IMAGE:?set HARNESS_IMAGE}"
REGION=${REGION:-iad}
HATCHET=v0.110.8
FORK_API_IMAGE=${FORK_API_IMAGE:-ghcr.io/rjpruitt16/hatchet-aquifer-api:$HATCHET-aqueduct-ff494eb}
WORKERS=${WORKERS:-3}
APP=aqb-hatchet-$NAME
TENANT=707d0855-80ab-4e1f-a156-f1c4546cbf52
OUT=results/$NAME
case "$CONFIG" in B | C | D | E) ;; *) echo "CONFIG must be B, C, D or E" >&2; exit 2 ;; esac
PG_SIZE=${PG_SIZE:-performance-2x}
# Memory and Postgres's caches follow the machine size.
case "$PG_SIZE" in
  performance-1x) PG_MEM=2048 PG_BUFFERS=512MB PG_CACHE=1536MB ;;
  performance-2x) PG_MEM=4096 PG_BUFFERS=1GB PG_CACHE=3GB ;;
  performance-4x) PG_MEM=8192 PG_BUFFERS=2GB PG_CACHE=6GB ;;
  *) echo "PG_SIZE must be performance-1x, -2x or -4x" >&2; exit 2 ;;
esac
mem_for() { case "$1" in performance-1x) echo 2048 ;; performance-2x) echo 4096 ;; performance-4x) echo 8192 ;; esac; }
for tool in fly jq openssl docker curl; do
  command -v "$tool" >/dev/null || { echo "run.sh needs $tool" >&2; exit 2; }
done
# Docker generates Hatchet's keys; check the daemon before creating anything.
docker info >/dev/null 2>&1 || { echo "the Docker daemon is not running" >&2; exit 2; }
umask 077
mkdir -p "$OUT"

# A failed run, or one whose results could not all be copied, keeps the app
# (its volumes and logs) for inspection instead of destroying it; so does any
# command that fails under set -e. Its machines are stopped so an unattended
# failure doesn't keep billing for CPU.
fail() {
  echo "$1" >&2
  exit 1
}

cleanup() {
  status=$?
  [ -n "${events_pid:-}" ] && kill "$events_pid" 2>/dev/null || true
  # The engine's and workers' logs go with the app; keep what Fly still has.
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
trap cleanup EXIT

machine_id() { fly machine list -a "$APP" --json | jq -r --arg n "$1" '.[] | select(.name == $n) | .id'; }
host() { echo "$(machine_id "$1").vm.$APP.internal"; }

# Waits until a one-shot machine has stopped with an exit event and prints its
# exit code. Fly omits exit_code from the event when it is 0, and lists events
# newest first. A failing fly call ends the run rather than reading as a hang.
wait_exit() {
  i=0
  while :; do
    list=$(fly machine list -a "$APP" --json) || fail "fly machine list failed while waiting for $1"
    code=$(echo "$list" | jq -r --arg n "$1" '.[] | select(.name == $n)
      | select(.state == "stopped" or .state == "destroyed")
      | [.events[]? | select(.type == "exit")][0] // empty
      | .request.exit_event.exit_code // 0')
    [ -n "$code" ] && break
    i=$((i + 1))
    [ "$i" -gt 180 ] && fail "$1 did not finish"
    sleep 5
  done
  echo "$code"
}

# Creates a volume and prints its ID once Fly reports it created; machines
# then attach it by ID. Attaching by name right after creating it can fail
# with "volume not found".
volume() {
  vid=$(fly volumes create "$1" -a "$APP" -r "$REGION" -s "$2" --vm-size "$3" -y --json | jq -r .id)
  i=0
  until [ "$(fly volumes show "$vid" -a "$APP" --json 2>/dev/null | jq -r .state)" = created ]; do
    i=$((i + 1))
    [ "$i" -gt 24 ] && fail "volume $1 was not created"
    sleep 5
  done
  echo "$vid"
}

# Fly's API times out now and then. retry repeats a call that is safe to
# repeat; create_app checks whether a timed-out create went through.
retry() {
  for try in 1 2 3 4; do
    "$@" && return 0
    sleep 10
  done
  return 1
}
create_app() {
  for try in 1 2 3 4; do
    fly apps create "$APP" --org "$FLY_ORG" >/dev/null && return 0
    sleep 10
    fly apps list --json 2>/dev/null | jq -e --arg a "$APP" 'any(.[]; .Name == $a)' >/dev/null && return 0
  done
  return 1
}

echo "== creating $APP in $FLY_ORG/$REGION ($CONFIG)"
create_app || fail "could not create $APP"

# Hatchet's keys, generated locally and stored as app secrets.
keys=$(mktemp -d)
docker run --rm -v "$keys:/keys" ghcr.io/hatchet-dev/hatchet/hatchet-admin:$HATCHET \
  /hatchet/hatchet-admin keyset create-local-keys --key-dir /keys >/dev/null
PGPW=$(openssl rand -hex 16)
retry fly secrets set -a "$APP" --stage \
  SERVER_ENCRYPTION_MASTER_KEYSET="$(cat "$keys/master.key")" \
  SERVER_ENCRYPTION_JWT_PRIVATE_KEYSET="$(cat "$keys/private_ec256.key")" \
  SERVER_ENCRYPTION_JWT_PUBLIC_KEYSET="$(cat "$keys/public_ec256.key")" \
  SERVER_AUTH_COOKIE_SECRETS="$(openssl rand -hex 8) $(openssl rand -hex 8)" \
  POSTGRES_PASSWORD="$PGPW" >/dev/null
rm -rf "$keys"

echo "== postgres"
# Place the volume on a host with room for the size Postgres may grow to.
PGVOL=$(volume pgdata 20 "${RESIZE_TO:-$PG_SIZE}")
fly machine run postgres:16.4 -a "$APP" -n pg -r "$REGION" --vm-size "$PG_SIZE" --vm-memory "$PG_MEM" \
  -v "$PGVOL":/var/lib/postgresql/data --restart no --detach \
  -e POSTGRES_USER=hatchet -e POSTGRES_DB=hatchet -e PGDATA=/var/lib/postgresql/data/pgdata \
  -- postgres -c listen_addresses='*' -c max_connections=400 -c shared_preload_libraries=pg_stat_statements \
  -c shared_buffers=$PG_BUFFERS -c effective_cache_size=$PG_CACHE -c work_mem=16MB >/dev/null
sleep 20
DB="postgres://hatchet:$PGPW@$(host pg):5432/hatchet?sslmode=disable"

HATCHET_ENV="-e DATABASE_URL=$DB -e SERVER_MSGQUEUE_KIND=postgres -e SERVER_AUTH_COOKIE_DOMAIN=localhost \
  -e SERVER_AUTH_COOKIE_INSECURE=t -e SERVER_AUTH_SET_EMAIL_VERIFIED=t -e SERVER_GRPC_BIND_ADDRESS=[::] \
  -e SERVER_GRPC_INSECURE=t -e SERVER_GRPC_PORT=7070 -e SERVER_PORT=8080 -e SERVER_URL=http://localhost:8080"

echo "== migrate and seed"
# A one-shot can exit before fly machine run sees it start, which fly reports
# as an error; wait_exit reads the real exit code instead.
fly machine run ghcr.io/hatchet-dev/hatchet/hatchet-migrate:$HATCHET /hatchet/hatchet-migrate -a "$APP" -n migrate \
  -r "$REGION" --vm-size shared-cpu-2x --restart no --detach -e DATABASE_URL="$DB" >/dev/null || true
[ "$(wait_exit migrate)" = 0 ] || fail "migrate failed"
# shellcheck disable=SC2086
fly machine run ghcr.io/hatchet-dev/hatchet/hatchet-admin:$HATCHET /hatchet/hatchet-admin seed -a "$APP" -n seed \
  -r "$REGION" --vm-size shared-cpu-2x --restart no --detach $HATCHET_ENV >/dev/null || true
[ "$(wait_exit seed)" = 0 ] || fail "seed failed"
# shellcheck disable=SC2086
fly machine run ghcr.io/hatchet-dev/hatchet/hatchet-admin:$HATCHET -a "$APP" -n token -r "$REGION" \
  --vm-size shared-cpu-2x --restart no --detach $HATCHET_ENV -- /hatchet/hatchet-admin token create --tenant-id $TENANT >/dev/null || true
[ "$(wait_exit token)" = 0 ] || fail "token create failed"
TOKEN=$(fly logs -a "$APP" --machine "$(machine_id token)" --no-tail 2>/dev/null | grep -oE 'ey[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' | tail -1)
[ -n "$TOKEN" ] || fail "could not read the API token"

echo "== hatchet engine and api"
# shellcheck disable=SC2086
# Tuned engine settings from TUNING.md; the calibration run sets the values.
ENGINE_TUNING="-e DATABASE_MAX_CONNS=${ENGINE_DB_CONNS:-100} -e SERVER_GRPC_TRIGGER_WRITE_SLOTS=${TRIGGER_SLOTS:-5} \
  -e SERVER_DEFAULT_BUFFER_SIZE=${BUFFER_SIZE:-10} -e SERVER_DEFAULT_BUFFER_FLUSH_INTERVAL=${BUFFER_FLUSH:-10ms}"
# shellcheck disable=SC2086
fly machine run ghcr.io/hatchet-dev/hatchet/hatchet-engine:$HATCHET /hatchet/hatchet-engine -a "$APP" -n engine \
  -r "$REGION" --vm-size performance-2x --vm-memory 4096 --restart on-failure --detach $HATCHET_ENV $ENGINE_TUNING >/dev/null
ENGINE_HOST=$(host engine)
API_IMAGE=ghcr.io/hatchet-dev/hatchet/hatchet-api:$HATCHET
API_SIZE="--vm-size performance-1x --vm-memory 2048"
API_PACING=""
if [ "$CONFIG" = D ]; then
  : # stock API at performance-1x; Envoy gets the other performance-1x
elif [ "$CONFIG" = C ] || [ "$CONFIG" = E ]; then
  API_IMAGE=$FORK_API_IMAGE
  if [ "${PACING:-}" = 1 ]; then
    API_PACING="-e AQUEDUCT_PACING_ENABLED=true -e AQUEDUCT_MAX_RPS=${PACING_MAX_RPS:-${RATE:-50}} -e AQUEDUCT_TARGET_DEPTH=${PACING_TARGET_DEPTH:-1000}${PACING_BACKLOG_SECONDS:+ -e AQUEDUCT_BACKLOG_SECONDS=$PACING_BACKLOG_SECONDS}"
  fi
else
  # B gets Aquifer's machine budget back on the API.
  API_SIZE="--vm-size performance-2x --vm-memory 4096"
fi
case "${API_VM:-}" in
  performance-1x) API_SIZE="--vm-size performance-1x --vm-memory 2048" ;;
  performance-2x) API_SIZE="--vm-size performance-2x --vm-memory 4096" ;;
esac
# shellcheck disable=SC2086
fly machine run "$API_IMAGE" /hatchet/hatchet-api -a "$APP" -n api -r "$REGION" $API_SIZE --restart on-failure --detach \
  $HATCHET_ENV -e SERVER_GRPC_BROADCAST_ADDRESS="$ENGINE_HOST:7070" \
  -e SERVER_INTERNAL_CLIENT_INTERNAL_GRPC_BROADCAST_ADDRESS="$ENGINE_HOST:7070" $API_PACING >/dev/null
sleep 20
API="http://$(host api):8080"

echo "== driver (created first so workers know its address; started last)"
RESVOL=$(volume results 1 performance-2x)
fly machine create "$HARNESS_IMAGE" -a "$APP" -n driver -r "$REGION" --vm-size performance-2x --vm-memory 4096 \
  -v "$RESVOL":/out --restart no >/dev/null
DRIVER_HOST=$(host driver)

if [ "$CONFIG" = C ] || [ "$CONFIG" = E ]; then
  echo "== aquifer"
  # Aquifer's job store lives on a volume, so jobs it has accepted survive a
  # machine restart.
  AQVOL=$(volume aqdata 5 performance-1x)
  aqcfg=$(mktemp)
  # Hatchet's API is paced at the calibrated rate. The driver receives
  # Aquifer's completion webhooks, so it must not fall under the small
  # default for unknown hosts.
  printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n  "%s:8080":\n    rps: %s\n    max_concurrent: %s\n  "%s:8080":\n    rps: 1000\n    max_concurrent: 64\n' \
    "$(host api)" "${AQUIFER_RPS:-${RATE:-50}}" "${AQUIFER_CONCURRENCY:-32}" "$DRIVER_HOST" > "$aqcfg"
  fly machine run "$HARNESS_IMAGE" -a "$APP" -n aquifer -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/aquifer.yaml="$aqcfg" -v "$AQVOL":/data \
    -e ROLE=aquifer -e AQUIFER_ADAPTER=http -e PORT=8080 -e DB_PATH=/data/aquifer -e CONFIG_PATH=/etc/aquifer.yaml \
    ${AQUIFER_FLUSH_MS:+-e AQUIFER_PEBBLE_FLUSH_INTERVAL_MS=$AQUIFER_FLUSH_MS} >/dev/null
  rm -f "$aqcfg"
  AQUIFER_HOST=$(host aquifer)
fi

if [ "$CONFIG" = D ] || [ "$CONFIG" = E ]; then
  echo "== envoy"
  # D: Envoy in front of Hatchet's API, with limits at Hatchet's capacity.
  # E: Envoy in front of Aquifer, with limits only against abuse; Aquifer
  # paces Hatchet, so Envoy doesn't also limit to capacity.
  if [ "$CONFIG" = D ]; then
    e_tenant=${TENANT_RPS:-15} e_global=${GLOBAL_RPS:-${RATE:-50}} e_upstream=$(host api)
  else
    e_tenant=${EDGE_TENANT_RPS:-100} e_global=${EDGE_GLOBAL_RPS:-400} e_upstream=$AQUIFER_HOST
  fi
  ecfg=$(mktemp)
  {
    for t in law-firm accounting small-1 small-2 small-3 small-4 small-5; do
      printf '                                - entries: [{ key: tenant, value: %s }]\n' "$t"
      printf '                                  token_bucket: { max_tokens: %s, tokens_per_fill: %s, fill_interval: 1s }\n' $((e_tenant * 2)) "$e_tenant"
    done
  } > "$ecfg.d"
  printf '              - endpoint: { address: { socket_address: { address: %s, port_value: 8080 } } }\n' "$e_upstream" > "$ecfg.e"
  sed -e "s/__GLOBAL_RPS__/$e_global/g" -e "/__DESCRIPTORS__/{r $ecfg.d
d;}" -e "/__ENDPOINTS__/{r $ecfg.e
d;}" "$(dirname "$0")/envoy.yaml.tmpl" > "$ecfg"
  sed "s/[0-9a-f]*\.vm\.$APP\.internal/<upstream>/" "$ecfg" > "$OUT/envoy.yaml"
  # --drain-time-s bounds a graceful drain (ENVOY_DRAIN_AT).
  fly machine run envoyproxy/envoy:v1.39.3 -a "$APP" -n envoy -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach --file-local /etc/envoy/envoy.yaml="$ecfg" \
    -- envoy -c /etc/envoy/envoy.yaml --drain-time-s "${ENVOY_DRAIN_S:-15}" >/dev/null
  rm -f "$ecfg" "$ecfg.d" "$ecfg.e"
  ENVOY_HOST=$(host envoy)
fi

echo "== workers"
i=1
while [ "$i" -le "$WORKERS" ]; do
  fly machine run "$HARNESS_IMAGE" -a "$APP" -n "worker-$i" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach -e ROLE=worker -e CONFIG=B -e SLOTS=200 -e TENANT_MAX_RUNS="${TENANT_MAX_RUNS:-50}" \
    -e CALLBACK_URL="http://$DRIVER_HOST:8080/done" -e HATCHET_CLIENT_TOKEN="$TOKEN" \
    -e HATCHET_CLIENT_TLS_STRATEGY=none -e HATCHET_CLIENT_HOST_PORT="$ENGINE_HOST:7070" \
    ${IDEMPOTENCY_TTL:+-e IDEMPOTENCY_TTL=$IDEMPOTENCY_TTL} >/dev/null
  i=$((i + 1))
done
sleep 20

echo "== driver"
set -- -e ROLE=driver -e HOLD=1 -e OUT=/out -e HATCHET_API="$API" -e HATCHET_CLIENT_TOKEN="$TOKEN" \
  -e DATABASE_URL="$DB" -e RATE="${RATE:-50}" -e LISTEN=:8080
[ -n "${PHASES:-}" ] && set -- "$@" -e PHASES="$PHASES"
[ -n "${SCALE:-}" ] && set -- "$@" -e SCALE="$SCALE"
[ -n "${DRAIN_MAX:-}" ] && set -- "$@" -e DRAIN_MAX="$DRAIN_MAX"
[ -n "${MAX_ATTEMPTS:-}" ] && set -- "$@" -e MAX_ATTEMPTS="$MAX_ATTEMPTS"
[ -n "${AQUIFER_MAX_RETRIES:-}" ] && set -- "$@" -e AQUIFER_MAX_RETRIES="$AQUIFER_MAX_RETRIES"
[ "$CONFIG" = D ] && set -- "$@" -e TRIGGER_API="http://$ENVOY_HOST:8080"
if [ "$CONFIG" = C ]; then
  set -- "$@" -e AQUIFER_URL="http://$AQUIFER_HOST:8080" -e PUBLIC_URL="http://$DRIVER_HOST:8080"
fi
# E: producers submit to Aquifer through Envoy; Aquifer's webhooks come
# straight back to the driver.
[ "$CONFIG" = E ] && set -- "$@" -e AQUIFER_URL="http://$ENVOY_HOST:8080" -e PUBLIC_URL="http://$DRIVER_HOST:8080"
# The driver samples the gateways' readiness and counters every 2 s, so
# counters that reset on a restart can be summed and recovery timed.
scrape=""
if [ -n "${ENVOY_HOST:-}" ]; then
  scrape="envoy-ready http://$ENVOY_HOST:9901/ready;envoy-stats http://$ENVOY_HOST:9901/stats?filter=%5E(cluster%5C.backend%5C.(upstream_rq_(total|retry|retry_success|5xx|timeout|pending_overflow|rx_reset|tx_reset)|upstream_cx_(total|destroy_remote_with_active_rq|destroy_local_with_active_rq))|http%5C.ingress%5C.downstream_rq_(total|5xx|4xx|2xx))%24 ."
fi
if [ -n "${AQUIFER_HOST:-}" ]; then
  scrape="${scrape:+$scrape;}aquifer-ready http://$AQUIFER_HOST:8080/health;aquifer-metrics http://$AQUIFER_HOST:8080/metrics ^aquifer_(dispatch_attempts_total|admission_rejected_total|wal_sync_lag_seconds)"
fi
[ -n "$scrape" ] && set -- "$@" -e SCRAPE="$scrape"
retry fly machine update "$(machine_id driver)" -a "$APP" -y --skip-start "$@" >/dev/null || fail "could not configure the driver"
retry fly machine start "$(machine_id driver)" -a "$APP" >/dev/null || fail "could not start the driver"
started=$(date -u +%FT%TZ)
started_s=$(date +%s)
echo "   started $started; waiting for the driver to finish"

# Scheduled changes to Postgres while the run goes on, each logged with the
# time it took effect.
if [ -n "${RESIZE_AT:-}${OUTAGE_AT:-}${ENVOY_KILL_AT:-}${ENVOY_DRAIN_AT:-}${AQUIFER_KILL_AT:-}${API_STOP_AT:-}" ]; then
  (
    pg=$(machine_id pg)
    log() { echo "$(date -u +%FT%TZ) $(( $(date +%s) - started_s ))s $*" >> "$OUT/events.txt"; }
    # until_s waits until this many seconds after the driver started.
    until_s() {
      w=$(( $1 - ($(date +%s) - started_s) ))
      [ "$w" -gt 0 ] && sleep "$w"
      return 0
    }
    # kill9 sends SIGKILL to every process named $2 on machine $1 (no
    # pidof or pkill in some images; /proc always works).
    kill9() {
      fly ssh console -a "$APP" --machine "$(machine_id "$1")" -C \
        "sh -c 'for p in /proc/[0-9]*; do [ \"\$(cat \$p/comm 2>/dev/null)\" = $2 ] && kill -9 \${p#/proc/}; done; true'" >/dev/null 2>&1
    }
    if [ -n "${ENVOY_KILL_AT:-}" ]; then
      until_s "$ENVOY_KILL_AT"
      log "envoy SIGKILL"
      kill9 envoy envoy && log "envoy killed" || log "envoy SIGKILL FAILED"
    fi
    if [ -n "${ENVOY_DRAIN_AT:-}" ]; then
      until_s "$ENVOY_DRAIN_AT"
      # Envoy's admin API, reached from the driver machine (Envoy's image has
      # no HTTP client): drain gracefully, then restart once the drain is done.
      log "envoy graceful drain"
      fly ssh console -a "$APP" --machine "$(machine_id driver)" -C \
        "wget -qO- --post-data= http://$ENVOY_HOST:9901/drain_listeners?graceful" >/dev/null 2>&1 && log "envoy draining" || log "envoy drain FAILED"
      sleep "${ENVOY_DRAIN_S:-15}"
      log "envoy restart"
      fly machine restart "$(machine_id envoy)" -a "$APP" >/dev/null 2>&1 && log "envoy restarted" || log "envoy restart FAILED"
    fi
    if [ -n "${AQUIFER_KILL_AT:-}" ]; then
      until_s "$AQUIFER_KILL_AT"
      log "aquifer SIGKILL"
      kill9 aquifer aquifer && log "aquifer killed" || log "aquifer SIGKILL FAILED"
    fi
    if [ -n "${API_STOP_AT:-}" ]; then
      until_s "$API_STOP_AT"
      log "hatchet api stop"
      fly machine stop "$(machine_id api)" -a "$APP" >/dev/null 2>&1 && log "hatchet api stopped" || log "hatchet api stop FAILED"
      sleep "${API_STOP_FOR:?set API_STOP_FOR}"
      log "hatchet api start"
      fly machine start "$(machine_id api)" -a "$APP" >/dev/null 2>&1 && log "hatchet api started" || log "hatchet api start FAILED"
    fi
    if [ -n "${RESIZE_AT:-}" ]; then
      sleep "$RESIZE_AT"
      log "resize to ${RESIZE_TO:?set RESIZE_TO} begins"
      if fly machine update "$pg" -a "$APP" -y --vm-size "$RESIZE_TO" --vm-memory "$(mem_for "$RESIZE_TO")" >/dev/null 2>&1; then
        log "resize to $RESIZE_TO done"
      else
        log "resize to $RESIZE_TO FAILED"
      fi
    fi
    if [ -n "${OUTAGE_AT:-}" ]; then
      wait_s=$(( OUTAGE_AT - ($(date +%s) - started_s) ))
      [ "$wait_s" -gt 0 ] && sleep "$wait_s"
      log "postgres stop"
      fly machine stop "$pg" -a "$APP" >/dev/null 2>&1 && log "postgres stopped" || log "postgres stop FAILED"
      sleep "${OUTAGE_FOR:?set OUTAGE_FOR}"
      log "postgres start"
      fly machine start "$pg" -a "$APP" >/dev/null 2>&1 && log "postgres started" || log "postgres start FAILED"
    fi
  ) &
  events_pid=$!
fi

# The driver writes /out/DONE and then holds the machine open so the results
# can be copied. Allow the full schedule plus drain.
i=0
until fly ssh console -a "$APP" --machine "$(machine_id driver)" -C "test -f /out/DONE" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 1800 ] && fail "driver did not finish within 5 hours"
  sleep 10
done

echo "== copying results to $OUT"
missing=""
for f in summary.json intervals.jsonl postgres.jsonl statements.json driver.log; do
  # sftp sometimes fails on a busy machine; retry before giving up.
  ok=""
  for try in 1 2 3 4 5; do
    fly ssh sftp get -a "$APP" --machine "$(machine_id driver)" "/out/$f" "$OUT/$f" >/dev/null 2>&1 && { ok=1; break; }
    rm -f "$OUT/$f"; sleep 10
  done
  [ -n "$ok" ] || missing="$missing $f"
done
fly ssh console -a "$APP" --machine "$(machine_id driver)" -C "cat /out/DONE" 2>/dev/null | tr -d "\r" > "$OUT/DONE" && [ -s "$OUT/DONE" ] || missing="$missing DONE"
[ -z "$missing" ] || fail "could not copy:$missing"
# Optional: scrape.jsonl only when the driver had gateways to sample, and
# duplicates.json only from a harness that counts Hatchet's runs per document.
for f in scrape.jsonl duplicates.json; do
  for try in 1 2 3; do
    fly ssh sftp get -a "$APP" --machine "$(machine_id driver)" "/out/$f" "$OUT/$f" >/dev/null 2>&1 && break
    rm -f "${OUT:?}/${f:?}"; sleep 5
  done
done
if [ "$CONFIG" = C ] || [ "$CONFIG" = E ]; then
  fly ssh console -a "$APP" --machine "$(machine_id aquifer)" -C "wget -qO- http://localhost:8080/metrics" 2>/dev/null \
    | grep -E '^aquifer_(admission_rejected_total|dispatch_attempts_total)' | sed 's/target="[^"]*"/target="hatchet-api"/' > "$OUT/aquifer_metrics.txt" || true
fi
# CPU per machine over the run, from Fly's metrics, for useful work per
# CPU-second: cores busy with work (user, system, interrupts; not idle,
# iowait or steal), in 30 s steps. fly_instance_cpu counts hundredths of a
# second. The token goes only in the request header.
case "${FLY_API_TOKEN:-}" in FlyV1*) auth="$FLY_API_TOKEN" ;; *) auth="Bearer ${FLY_API_TOKEN:-}" ;; esac
q="sum by (instance) (rate(fly_instance_cpu{app=\"$APP\",mode=~\"user|nice|system|irq|softirq\"}[1m])) / 100"
curl -s -G -H "Authorization: $auth" "https://api.fly.io/prometheus/$FLY_ORG/api/v1/query_range" \
  --data-urlencode "query=$q" --data-urlencode "start=$started_s" --data-urlencode "end=$(date +%s)" \
  --data-urlencode "step=30" > "$OUT/cpu.raw.json" 2>/dev/null || true
fly machine list -a "$APP" --json | jq -r '[.[] | {(.id): .name}] | add' > "$OUT/cpu.names.json" 2>/dev/null || true
jq -n --slurpfile raw "$OUT/cpu.raw.json" --slurpfile names "$OUT/cpu.names.json" \
  '{unit: "cores", machines: [$raw[0].data.result[]? | {machine: ($names[0][.metric.instance] // .metric.instance), values}]}' \
  > "$OUT/cpu.json" 2>/dev/null || true
rm -f "$OUT/cpu.raw.json" "$OUT/cpu.names.json"
# Machine sizes and images only: the full config holds the token and the
# database password.
fly machine list -a "$APP" --json \
  | jq '[.[] | {id, name, region, image: .config.image, guest: .config.guest}]' > "$OUT/machines.json"
{
  echo "config=$CONFIG"
  echo "started=$started"
  echo "hatchet=$HATCHET"
  echo "harness_image=$HARNESS_IMAGE"
  case "$CONFIG" in C | E) echo "api_image=$FORK_API_IMAGE" ;; esac
  echo "api_vm=${API_VM:-default}"
  echo "region=$REGION"
  echo "workers=$WORKERS"
  echo "rate=${RATE:-50}"
  echo "tenant_max_runs=${TENANT_MAX_RUNS:-50}"
  echo "engine_db_conns=${ENGINE_DB_CONNS:-100} trigger_slots=${TRIGGER_SLOTS:-5} buffer_size=${BUFFER_SIZE:-10} buffer_flush=${BUFFER_FLUSH:-10ms}"
  [ -n "${PHASES:-}" ] && echo "phases=$PHASES"
  echo "drain_max=${DRAIN_MAX:-30m}"
  case "$CONFIG" in C | E) echo "aquifer_rps=${AQUIFER_RPS:-${RATE:-50}} aquifer_concurrency=${AQUIFER_CONCURRENCY:-32}"; echo "aquifer_flush_ms=${AQUIFER_FLUSH_MS:-default}" ;; esac
  [ -n "$API_PACING" ] && echo "pacing=on target_depth=${PACING_TARGET_DEPTH:-1000} max_rps=${PACING_MAX_RPS:-${RATE:-50}} backlog_seconds=${PACING_BACKLOG_SECONDS:-60}"
  echo "pg_size=$PG_SIZE"
  echo "idempotency_ttl=${IDEMPOTENCY_TTL:-off}"
  [ -n "${MAX_ATTEMPTS:-}" ] && echo "max_attempts=$MAX_ATTEMPTS"
  [ -n "${AQUIFER_MAX_RETRIES:-}" ] && echo "aquifer_max_retries=$AQUIFER_MAX_RETRIES"
  [ "$CONFIG" = D ] && echo "envoy_tenant_rps=${TENANT_RPS:-15} envoy_global_rps=${GLOBAL_RPS:-${RATE:-50}}"
  [ "$CONFIG" = E ] && echo "envoy_tenant_rps=${EDGE_TENANT_RPS:-100} envoy_global_rps=${EDGE_GLOBAL_RPS:-400}"
  [ -n "${ENVOY_KILL_AT:-}" ] && echo "envoy_kill_at=$ENVOY_KILL_AT"
  [ -n "${ENVOY_DRAIN_AT:-}" ] && echo "envoy_drain_at=$ENVOY_DRAIN_AT envoy_drain_s=${ENVOY_DRAIN_S:-15}"
  [ -n "${AQUIFER_KILL_AT:-}" ] && echo "aquifer_kill_at=$AQUIFER_KILL_AT"
  [ -n "${API_STOP_AT:-}" ] && echo "api_stop_at=$API_STOP_AT api_stop_for=$API_STOP_FOR"
  [ -n "${RESIZE_AT:-}" ] && echo "resize_at=$RESIZE_AT resize_to=$RESIZE_TO"
  [ -n "${OUTAGE_AT:-}" ] && echo "outage_at=$OUTAGE_AT outage_for=$OUTAGE_FOR"
} > "$OUT/run.txt"
status=$(cat "$OUT/DONE")
[ "$status" = 0 ] || fail "driver exited with status $status; see $OUT/driver.log"
echo "== done: $OUT"
