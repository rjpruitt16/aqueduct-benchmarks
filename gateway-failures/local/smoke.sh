#!/bin/sh
# Local smoke test for the gateway-failures experiment: one configuration,
# one failure, on one machine with Docker and Go, against local Hatchet
# (hatchet/deploy/local/setup.sh). It checks the plumbing and the
# accounting, not performance: a short schedule at a low rate.
#
#   A  Envoy -> Hatchet
#   B  Envoy -> Aquifer -> Hatchet
#   C  Aquifer -> Hatchet
#
# Failures, injected FAIL_AT seconds into the run (default 50, in the burst):
#   envoy-kill    SIGKILL Envoy's process; Docker's restart policy brings it back (A, B)
#   envoy-drain   drain Envoy's listeners gracefully, wait ENVOY_DRAIN_S (5), restart it (A, B)
#   aquifer-kill  SIGKILL Aquifer; the supervisor loop below restarts it on the same data (B, C)
#   api-stop      stop Hatchet's API container for API_STOP_FOR seconds (20) (all)
#
# Usage, from the repository root: gateway-failures/local/smoke.sh <A|B|C> <failure>
# Results: .smoke/<config>-<failure>/out (summary.json, scrape.jsonl, events.txt, ...).
set -eu
CONFIG=${1:?usage: smoke.sh <A|B|C> <envoy-kill|envoy-drain|aquifer-kill|api-stop>}
FAILURE=${2:?usage: smoke.sh <A|B|C> <envoy-kill|envoy-drain|aquifer-kill|api-stop>}
case "$CONFIG:$FAILURE" in
  A:envoy-kill | A:envoy-drain | A:api-stop | B:* | C:aquifer-kill | C:api-stop) ;;
  *) echo "$FAILURE doesn't apply to $CONFIG" >&2; exit 2 ;;
esac
ROOT=$(git rev-parse --show-toplevel)
W=$ROOT/.smoke/$CONFIG-$FAILURE
BIN=$ROOT/.smoke/bin
LOCAL=$ROOT/hatchet/deploy/local
AQUIFER_REF=${AQUIFER_REF:-630501d}
FAIL_AT=${FAIL_AT:-50}
export COMPOSE_FILE=${COMPOSE_FILE:-compose.yml}

rm -rf "${W:?}"
mkdir -p "$W/out" "$BIN"
printf 'config=%s\nscenario=%s\nfail_at=%s\nlocal=1\n' "$CONFIG" "$FAILURE" "$FAIL_AT" > "$W/out/run.txt"
TOKEN=$(cat "$LOCAL/token.txt")

# Binaries: the harness from this checkout, Aquifer at the ref the Fly runs use.
(cd "$ROOT/hatchet" && go build -o "$BIN/" ./cmd/...)
AQSRC=$ROOT/.smoke/aquifer-src
[ -d "$AQSRC/.git" ] || git clone -q https://github.com/rjpruitt16/aquifer "$AQSRC"
(cd "$AQSRC" && git fetch -q origin && git checkout -q "$AQUIFER_REF" && go build -o "$BIN/aquifer" ./cmd/aquifer)

pids=""
cleanup() {
  touch "$W/stop"
  for p in $pids; do kill "$p" 2>/dev/null || true; done
  [ -n "${aq_pid_file:-}" ] && [ -s "$aq_pid_file" ] && kill "$(cat "$aq_pid_file")" 2>/dev/null || true
  docker rm -f gw-envoy >/dev/null 2>&1 || true
  (cd "$LOCAL" && docker compose start api >/dev/null 2>&1) || true
}
trap cleanup EXIT
log() { echo "$(date -u +%FT%TZ) $(( $(date +%s) - start_s ))s $*" >> "$W/out/events.txt"; }

# Aquifer (B, C), restarted on exit like Fly's restart policy, on one data dir.
AQ=""
if [ "$CONFIG" != A ]; then
  printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n  "127.0.0.1:8080":\n    rps: 4\n    max_concurrent: 16\n  "127.0.0.1:18080":\n    rps: 1000\n    max_concurrent: 64\n' > "$W/aquifer.yaml"
  aq_pid_file=$W/aquifer.pid
  (
    while [ ! -f "$W/stop" ]; do
      AQUIFER_ADAPTER=http PORT=18090 DB_PATH="$W/aqdata" CONFIG_PATH="$W/aquifer.yaml" "$BIN/aquifer" >> "$W/aquifer.log" 2>&1 &
      echo $! > "$aq_pid_file"
      wait $! || true
      [ -f "$W/stop" ] || echo "$(date -u +%FT%TZ) aquifer exited; restarting" >> "$W/aquifer.log"
      sleep 1
    done
  ) &
  pids="$pids $!"
  AQ=http://127.0.0.1:18090
fi

# Envoy (A, B): the Fly template, on local ports, with a restart policy.
ENVOY=""
if [ "$CONFIG" != C ]; then
  if [ "$CONFIG" = A ]; then tenant=3 global=4 upstream=127.0.0.1 port=8080; else tenant=100 global=400 upstream=127.0.0.1 port=18090; fi
  for t in law-firm accounting small-1 small-2 small-3 small-4 small-5; do
    printf '                                - entries: [{ key: tenant, value: %s }]\n' "$t"
    printf '                                  token_bucket: { max_tokens: %s, tokens_per_fill: %s, fill_interval: 1s }\n' $((tenant * 2)) "$tenant"
  done > "$W/envoy.d"
  printf '              - endpoint: { address: { socket_address: { address: %s, port_value: %s } } }\n' "$upstream" "$port" > "$W/envoy.e"
  sed -e "s/__GLOBAL_RPS__/$global/g" -e "/__DESCRIPTORS__/{r $W/envoy.d
d;}" -e "/__ENDPOINTS__/{r $W/envoy.e
d;}" -e 's/port_value: 8080, ipv4_compat/port_value: 18081, ipv4_compat/' -e 's/port_value: 9901/port_value: 19901/' \
    -e 's/V6_ONLY/V4_ONLY/' "$ROOT/hatchet/deploy/fly/envoy.yaml.tmpl" > "$W/envoy.yaml"
  docker rm -f gw-envoy >/dev/null 2>&1 || true
  docker run -d --name gw-envoy --network host --restart on-failure -v "$W/envoy.yaml:/etc/envoy/envoy.yaml:ro" \
    envoyproxy/envoy:v1.39.3 envoy -c /etc/envoy/envoy.yaml --drain-time-s "${ENVOY_DRAIN_S:-5}" >/dev/null
  ENVOY=http://127.0.0.1:18081
fi

# Wait for what this configuration needs.
i=0
until { [ -z "$AQ" ] || curl -sf "$AQ/health" >/dev/null; } && { [ -z "$ENVOY" ] || curl -sf http://127.0.0.1:19901/ready >/dev/null; }; do
  i=$((i + 1)); [ "$i" -gt 60 ] && { echo "gateways didn't come up" >&2; exit 1; }; sleep 1
done

# Workers.
IDEMPOTENCY_TTL=2h SLOTS=50 TENANT_MAX_RUNS=50 CONFIG=B CALLBACK_URL=http://127.0.0.1:18080/done HATCHET_CLIENT_TOKEN="$TOKEN" \
  HATCHET_CLIENT_TLS_STRATEGY=none HATCHET_CLIENT_HOST_PORT=127.0.0.1:7077 "$BIN/worker" > "$W/worker.log" 2>&1 &
pids="$pids $!"
sleep 5

# The driver: the Fly runs' settings, at a low rate and a short schedule.
scrape=""
[ -n "$ENVOY" ] && scrape="envoy-ready http://127.0.0.1:19901/ready;envoy-stats http://127.0.0.1:19901/stats?filter=%5E(cluster%5C.backend%5C.(upstream_rq_(total|retry|retry_success|5xx|timeout|pending_overflow|rx_reset|tx_reset)|upstream_cx_(total|destroy_remote_with_active_rq|destroy_local_with_active_rq))|http%5C.ingress%5C.downstream_rq_(total|5xx|4xx|2xx))%24 ."
[ -n "$AQ" ] && scrape="${scrape:+$scrape;}aquifer-ready $AQ/health;aquifer-metrics $AQ/metrics ^aquifer_(dispatch_attempts_total|admission_rejected_total|wal_sync_lag_seconds)"
set -- RATE=4 PHASES=steady:20:0.5,burst:40:3,sustained:40:1.5,recovery:40:0.5 MAX_ATTEMPTS=1000000 AQUIFER_MAX_RETRIES=-1 \
  DRAIN_MAX=4m OUT="$W/out" LISTEN=:18080 HATCHET_API=http://127.0.0.1:8080 HATCHET_CLIENT_TOKEN="$TOKEN" SCRAPE="$scrape"
case "$CONFIG" in
  A) set -- "$@" TRIGGER_API="$ENVOY" ;;
  B) set -- "$@" AQUIFER_URL="$ENVOY" PUBLIC_URL=http://127.0.0.1:18080 ;;
  C) set -- "$@" AQUIFER_URL="$AQ" PUBLIC_URL=http://127.0.0.1:18080 ;;
esac
start_s=$(date +%s)
env "$@" "$BIN/driver" > "$W/out/driver.log" 2>&1 &
driver_pid=$!
# Postgres sampler, as on Fly: its exit writes duplicates.json.
DATABASE_URL="postgres://hatchet:hatchet@127.0.0.1:5433/hatchet?sslmode=disable" OUT="$W/out" "$BIN/pgsampler" > "$W/pgsampler.log" 2>&1 &
sampler_pid=$!

# The failure.
sleep "$FAIL_AT"
log "$FAILURE begins"
case "$FAILURE" in
  envoy-kill)
    sudo -n kill -9 "$(docker inspect -f '{{.State.Pid}}' gw-envoy)" && log "envoy SIGKILL sent" ;;
  envoy-drain)
    curl -s -X POST "http://127.0.0.1:19901/drain_listeners?graceful" >/dev/null && log "envoy draining"
    sleep "${ENVOY_DRAIN_S:-5}"
    docker restart gw-envoy >/dev/null && log "envoy restarted" ;;
  aquifer-kill)
    kill -9 "$(cat "$aq_pid_file")" && log "aquifer SIGKILL sent" ;;
  api-stop)
    (cd "$LOCAL" && docker compose stop api >/dev/null 2>&1) && log "hatchet api stopped"
    sleep "${API_STOP_FOR:-20}"
    (cd "$LOCAL" && docker compose start api >/dev/null 2>&1) && log "hatchet api started" ;;
esac

status=0
wait "$driver_pid" || status=$?
kill -INT "$sampler_pid" 2>/dev/null || true
wait "$sampler_pid" 2>/dev/null || true
[ "$status" = 0 ] || { echo "driver exited non-zero; see $W/out/driver.log" >&2; exit 1; }
echo "done: $W/out"
