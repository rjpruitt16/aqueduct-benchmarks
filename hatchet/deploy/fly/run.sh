#!/bin/sh
# One measured Hatchet benchmark run on Fly.io: creates a private app, runs
# Postgres, Hatchet (engine and API), workers, optionally Aquifer, and the
# driver with the Postgres sampler; copies the results out; destroys the app.
#
# Usage: deploy/fly/run.sh <B|C> <run-name>
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
#   ENGINE_DB_CONNS, TRIGGER_SLOTS, BUFFER_SIZE, BUFFER_FLUSH
#                   tuned engine settings (see TUNING.md); fixed by calibration
#
# Machines (all dedicated CPUs, so runs are repeatable):
#   pg       performance-2x, 4 GB, 20 GB volume   Postgres 16 (also Hatchet's message queue)
#   engine   performance-2x, 4 GB                 Hatchet engine
#   api      performance-1x, 2 GB                 Hatchet REST API (the fork's image in C)
#   worker-N performance-1x, 2 GB                 pipeline workers
#   aquifer  performance-1x, 2 GB                 C only; in B the API gets the same budget back
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
case "$CONFIG" in B | C) ;; *) echo "CONFIG must be B or C" >&2; exit 2 ;; esac
for tool in fly jq openssl docker; do
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

echo "== creating $APP in $FLY_ORG/$REGION ($CONFIG)"
fly apps create "$APP" --org "$FLY_ORG" >/dev/null

# Hatchet's keys, generated locally and stored as app secrets.
keys=$(mktemp -d)
docker run --rm -v "$keys:/keys" ghcr.io/hatchet-dev/hatchet/hatchet-admin:$HATCHET \
  /hatchet/hatchet-admin keyset create-local-keys --key-dir /keys >/dev/null
PGPW=$(openssl rand -hex 16)
fly secrets set -a "$APP" --stage \
  SERVER_ENCRYPTION_MASTER_KEYSET="$(cat "$keys/master.key")" \
  SERVER_ENCRYPTION_JWT_PRIVATE_KEYSET="$(cat "$keys/private_ec256.key")" \
  SERVER_ENCRYPTION_JWT_PUBLIC_KEYSET="$(cat "$keys/public_ec256.key")" \
  SERVER_AUTH_COOKIE_SECRETS="$(openssl rand -hex 8) $(openssl rand -hex 8)" \
  POSTGRES_PASSWORD="$PGPW" >/dev/null
rm -rf "$keys"

echo "== postgres"
PGVOL=$(volume pgdata 20 performance-2x)
fly machine run postgres:16.4 -a "$APP" -n pg -r "$REGION" --vm-size performance-2x --vm-memory 4096 \
  -v "$PGVOL":/var/lib/postgresql/data --restart no --detach \
  -e POSTGRES_USER=hatchet -e POSTGRES_DB=hatchet -e PGDATA=/var/lib/postgresql/data/pgdata \
  -- postgres -c listen_addresses='*' -c max_connections=400 -c shared_preload_libraries=pg_stat_statements \
  -c shared_buffers=1GB -c effective_cache_size=3GB -c work_mem=16MB >/dev/null
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
if [ "$CONFIG" = C ]; then
  API_IMAGE=$FORK_API_IMAGE
else
  # B gets Aquifer's machine budget back on the API.
  API_SIZE="--vm-size performance-2x --vm-memory 4096"
fi
# shellcheck disable=SC2086
fly machine run "$API_IMAGE" /hatchet/hatchet-api -a "$APP" -n api -r "$REGION" $API_SIZE --restart on-failure --detach \
  $HATCHET_ENV -e SERVER_GRPC_BROADCAST_ADDRESS="$ENGINE_HOST:7070" \
  -e SERVER_INTERNAL_CLIENT_INTERNAL_GRPC_BROADCAST_ADDRESS="$ENGINE_HOST:7070" >/dev/null
sleep 20
API="http://$(host api):8080"

echo "== driver (created first so workers know its address; started last)"
RESVOL=$(volume results 1 performance-2x)
fly machine create "$HARNESS_IMAGE" -a "$APP" -n driver -r "$REGION" --vm-size performance-2x --vm-memory 4096 \
  -v "$RESVOL":/out --restart no >/dev/null
DRIVER_HOST=$(host driver)

if [ "$CONFIG" = C ]; then
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

echo "== workers"
i=1
while [ "$i" -le "$WORKERS" ]; do
  fly machine run "$HARNESS_IMAGE" -a "$APP" -n "worker-$i" -r "$REGION" --vm-size performance-1x --vm-memory 2048 \
    --restart on-failure --detach -e ROLE=worker -e CONFIG=B -e SLOTS=200 -e TENANT_MAX_RUNS="${TENANT_MAX_RUNS:-50}" \
    -e CALLBACK_URL="http://$DRIVER_HOST:8080/done" -e HATCHET_CLIENT_TOKEN="$TOKEN" \
    -e HATCHET_CLIENT_TLS_STRATEGY=none -e HATCHET_CLIENT_HOST_PORT="$ENGINE_HOST:7070" >/dev/null
  i=$((i + 1))
done
sleep 20

echo "== driver"
set -- -e ROLE=driver -e HOLD=1 -e OUT=/out -e HATCHET_API="$API" -e HATCHET_CLIENT_TOKEN="$TOKEN" \
  -e DATABASE_URL="$DB" -e RATE="${RATE:-50}" -e LISTEN=:8080
[ -n "${PHASES:-}" ] && set -- "$@" -e PHASES="$PHASES"
[ -n "${SCALE:-}" ] && set -- "$@" -e SCALE="$SCALE"
[ -n "${DRAIN_MAX:-}" ] && set -- "$@" -e DRAIN_MAX="$DRAIN_MAX"
if [ "$CONFIG" = C ]; then
  set -- "$@" -e AQUIFER_URL="http://$AQUIFER_HOST:8080" -e PUBLIC_URL="http://$DRIVER_HOST:8080"
fi
fly machine update "$(machine_id driver)" -a "$APP" -y --skip-start "$@" >/dev/null
fly machine start "$(machine_id driver)" -a "$APP" >/dev/null
started=$(date -u +%FT%TZ)
echo "   started $started; waiting for the driver to finish"

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
  fly ssh sftp get -a "$APP" --machine "$(machine_id driver)" "/out/$f" "$OUT/$f" >/dev/null 2>&1 || missing="$missing $f"
done
fly ssh console -a "$APP" --machine "$(machine_id driver)" -C "cat /out/DONE" 2>/dev/null | tr -d "\r" > "$OUT/DONE" && [ -s "$OUT/DONE" ] || missing="$missing DONE"
[ -z "$missing" ] || fail "could not copy:$missing"
if [ "$CONFIG" = C ]; then
  fly ssh console -a "$APP" --machine "$(machine_id aquifer)" -C "wget -qO- http://localhost:8080/metrics" 2>/dev/null \
    | grep -E '^aquifer_(admission_rejected_total|dispatch_attempts_total)' | sed 's/target="[^"]*"/target="hatchet-api"/' > "$OUT/aquifer_metrics.txt" || true
fi
# Machine sizes and images only: the full config holds the token and the
# database password.
fly machine list -a "$APP" --json \
  | jq '[.[] | {id, name, region, image: .config.image, guest: .config.guest}]' > "$OUT/machines.json"
{
  echo "config=$CONFIG"
  echo "started=$started"
  echo "hatchet=$HATCHET"
  echo "harness_image=$HARNESS_IMAGE"
  [ "$CONFIG" = C ] && echo "api_image=$FORK_API_IMAGE"
  echo "region=$REGION"
  echo "workers=$WORKERS"
  echo "rate=${RATE:-50}"
  echo "tenant_max_runs=${TENANT_MAX_RUNS:-50}"
  echo "engine_db_conns=${ENGINE_DB_CONNS:-100} trigger_slots=${TRIGGER_SLOTS:-5} buffer_size=${BUFFER_SIZE:-10} buffer_flush=${BUFFER_FLUSH:-10ms}"
  [ -n "${PHASES:-}" ] && echo "phases=$PHASES"
  echo "drain_max=${DRAIN_MAX:-30m}"
  [ "$CONFIG" = C ] && echo "aquifer_rps=${AQUIFER_RPS:-${RATE:-50}} aquifer_concurrency=${AQUIFER_CONCURRENCY:-32}"
  [ "$CONFIG" = C ] && echo "aquifer_flush_ms=${AQUIFER_FLUSH_MS:-default}"
} > "$OUT/run.txt"
status=$(cat "$OUT/DONE")
[ "$status" = 0 ] || fail "driver exited with status $status; see $OUT/driver.log"
echo "== done: $OUT"
