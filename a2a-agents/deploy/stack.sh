#!/bin/sh
# One measured run, inside the pilot machine (deploy/Dockerfile): configuration
# A, B or C, one scenario, from a clean state, results to $OUT.
#
# Everything but the model runs here: the agents, the database tool with its
# own Postgres, the sandbox (nsjail), mock CI, Redis and the dispatcher (B),
# Aquifer (C), and the driver. The model is vLLM elsewhere (LLM_TARGET), reached
# through cmd/advertise on this machine in every configuration, or the
# stand-in model when LLM_TARGET is empty (rehearsals).
#
# Usage: stack.sh <A|B|C>
#
# Environment:
#   OUT          results directory (required)
#   PHASES       the driver's load (required), e.g. steady:480:0.2
#   TENANTS      default tenant weights (big:3,small-1:1,small-2:1)
#   DRAIN        how long workflows may run on after the load ends (4m)
#   LLM_TARGET   vLLM's base URL; empty runs the stand-in model
#   LLM_MODEL    the model name vLLM serves (Qwen/Qwen2.5-7B-Instruct)
#   LLM_MAX      the model's capacity in sequences (vLLM's --max-num-seqs; 8)
#   DB_POOL (8), SLOTS (4), CI_RUNNERS (4), CI_DURATION (30s-90s), CI_FLAKY_PCT (5),
#   CI_RATE_PER_MIN (60), CI_BURST (10), CI_POLL (10s)
#   STANDIN_TOKENS_PER_S  the stand-in model's speed per sequence (40)
set -eu
CONFIG=${1:?usage: stack.sh <A|B|C>}
case "$CONFIG" in A) MODE=direct ;; B) MODE=queue ;; C) MODE=aquifer ;; *) echo "config must be A, B or C" >&2; exit 2 ;; esac
: "${OUT:?set OUT}"
: "${PHASES:?set PHASES}"
TENANTS=${TENANTS:-big:3,small-1:1,small-2:1}
LLM_MAX=${LLM_MAX:-8}
DB_POOL=${DB_POOL:-8}
SLOTS=${SLOTS:-4}
CI_RUNNERS=${CI_RUNNERS:-4}
W=/tmp/stack
rm -rf "$W"
mkdir -p "$W" "$OUT"
cat > "$OUT/run.txt" <<EOF
config=$CONFIG
mode=$MODE
phases=$PHASES
tenants=$TENANTS
llm=${LLM_TARGET:-stand-in}
llm_model=${LLM_MODEL:-Qwen/Qwen2.5-7B-Instruct}
llm_max=$LLM_MAX
db_pool=$DB_POOL
slots=$SLOTS
ci=${CI_RUNNERS} runners, ${CI_DURATION:-30s-90s}, ${CI_FLAKY_PCT:-5}% flaky, ${CI_RATE_PER_MIN:-60}/min burst ${CI_BURST:-10}, poll ${CI_POLL:-10s}
aquifer=$(cat /usr/local/share/aquifer-ref 2>/dev/null || echo unknown)
image=$(cat /usr/local/share/image-ref 2>/dev/null || echo unknown)
started=$(date -u +%FT%TZ)
EOF

pids=""
cleanup() {
  for p in $pids; do kill "$p" 2>/dev/null || true; done
  su postgres -c "$PGBIN/pg_ctl -D $W/pg -m immediate stop" >/dev/null 2>&1 || true
  redis-cli -p 16379 shutdown nosave >/dev/null 2>&1 || true
}
trap cleanup EXIT
# start <name> <program> [VAR=value...]: run program with those settings.
start() { name=$1 prog=$2; shift 2; env "$@" "$prog" > "$OUT/$name.log" 2>&1 & pids="$pids $!"; }
wait_for() { i=0; until curl -sf "$1" >/dev/null; do i=$((i + 1)); [ "$i" -gt "${2:-60}" ] && { echo "$1 didn't come up" >&2; exit 1; }; sleep 1; done; }

# A fresh Postgres for the database tool.
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | tail -1)
mkdir -p "$W/pg" && chown postgres "$W/pg"
su postgres -c "$PGBIN/initdb -D $W/pg -A trust -U postgres" >/dev/null
su postgres -c "$PGBIN/pg_ctl -D $W/pg -o '-p 15432 -k /tmp' -l $W/pg.log -w start" >/dev/null
su postgres -c "$PGBIN/createdb -h /tmp -p 15432 -U postgres a2a"
if [ "$CONFIG" = B ]; then
  redis-server --port 16379 --save '' --appendonly no --daemonize yes >/dev/null
fi

if [ -n "${LLM_TARGET:-}" ]; then
  start advertise advertise LISTEN=:18000 TARGET="$LLM_TARGET" MAX_CONCURRENT="$LLM_MAX" API_KEY="${LLM_API_KEY:-}"
else
  start mockllm mockllm LISTEN=:18000 MAX_SEQS="$LLM_MAX" TOKENS_PER_S="${STANDIN_TOKENS_PER_S:-40}"
fi
start dbtool dbtool LISTEN=:18100 DATABASE_URL="postgres://postgres@127.0.0.1:15432/a2a?sslmode=disable" DB_POOL="$DB_POOL" DB_WAIT=500ms
start sandbox sandbox LISTEN=:18200 SLOTS="$SLOTS" RUNNER=nsjail EXEC_TIMEOUT=60s LOG="$OUT/executions.jsonl"
start mockci mockci LISTEN=:18500 RUNNERS="$CI_RUNNERS" DURATION="${CI_DURATION:-30s-90s}" FLAKY_PCT="${CI_FLAKY_PCT:-5}" \
  RATE_PER_MIN="${CI_RATE_PER_MIN:-60}" BURST="${CI_BURST:-10}"
LLM=http://127.0.0.1:18000 DB=http://127.0.0.1:18100 SBX=http://127.0.0.1:18200 CI=http://127.0.0.1:18500
metrics="llm=$LLM/metrics,dbtool=$DB/metrics,sandbox=$SBX/metrics,mockci=$CI/metrics"
extra=""
case "$CONFIG" in
  B)
    start dispatcher dispatcher LISTEN=:18300 REDIS_URL=redis://127.0.0.1:16379 RESOURCES="llm=$LLM_MAX,db=$DB_POOL,sandbox=$SLOTS,ci=$CI_RUNNERS"
    extra="REDIS_URL=redis://127.0.0.1:16379"
    metrics="$metrics,dispatcher=http://127.0.0.1:18300/metrics" ;;
  C)
    # The same ceilings B's dispatcher has. With shared destination limits,
    # Aquifer holds each resource's advertised total across tenant queues.
    cat > "$W/aquifer.yaml" <<EOF
defaults:
  rps: 2
  max_concurrent: 1
upstreams:
  "127.0.0.1:18000":
    rps: 1000
    max_concurrent: $LLM_MAX
  "127.0.0.1:18100":
    rps: 1000
    max_concurrent: $DB_POOL
  "127.0.0.1:18200":
    rps: 1000
    max_concurrent: $SLOTS
  "127.0.0.1:18500":
    rps: 1000
    max_concurrent: $CI_RUNNERS
EOF
    cp "$W/aquifer.yaml" "$OUT/aquifer.yaml"
    # Test runs and model calls take longer than /proxy's default 3 s direct
    # attempt, which would cut them off and run them again.
    start aquifer aquifer AQUIFER_ADAPTER=http PORT=18400 DB_PATH="$W/aqdata" CONFIG_PATH="$W/aquifer.yaml" \
      AQUIFER_PROXY_DIRECT_ATTEMPT_TIMEOUT_SECONDS=120 AQUIFER_SHARED_DESTINATION_LIMITS=true
    extra="AQUIFER_URL=http://127.0.0.1:18400"
    metrics="$metrics,aquifer=http://127.0.0.1:18400/metrics" ;;
esac
for a in research:19001 coding:19002 verify:19003; do
  start "${a%%:*}" agent ROLE="${a%%:*}" LISTEN=":${a#*:}" PUBLIC_URL="http://127.0.0.1:${a#*:}" \
    RESOURCE_MODE=$MODE LLM_URL=$LLM LLM_MODEL="${LLM_MODEL:-Qwen/Qwen2.5-7B-Instruct}" DB_URL=$DB SANDBOX_URL=$SBX \
    CI_URL=$CI CI_POLL="${CI_POLL:-10s}" $extra
done
start coordinator agent ROLE=coordinator LISTEN=:19000 PUBLIC_URL=http://127.0.0.1:19000 \
  RESEARCH_URL=http://127.0.0.1:19001 CODING_URL=http://127.0.0.1:19002 VERIFY_URL=http://127.0.0.1:19003

for u in $DB/health $SBX/health $CI/health; do wait_for "$u"; done
if [ -n "${LLM_TARGET:-}" ]; then wait_for "$LLM/v1/models" 120; else wait_for "$LLM/health"; fi
[ "$CONFIG" = C ] && wait_for http://127.0.0.1:18400/health
for p in 19000 19001 19002 19003; do wait_for "http://127.0.0.1:$p/.well-known/agent-card.json" 30; done

# Every 5 s: the model's queue, the resources' in-flight counts and the
# machine's CPU, for the report's time series.
(
  while :; do
    echo "# t=$(date +%s)"
    curl -s -m 2 "$LLM/metrics" | grep -E '^vllm:(num_requests_(running|waiting)|gpu_cache_usage_perc|generation_tokens_total)' || true
    curl -s -m 2 "$SBX/metrics" | grep -E '^sandbox_(slots_busy|refused_total|executions_total)' || true
    curl -s -m 2 "$CI/metrics" | grep -E '^ci_(queued|running|refused_total|polls_total)' || true
    head -1 /proc/stat
    sleep 5
  done
) > "$OUT/samples.txt" 2>&1 &
pids="$pids $!"

PHASES=$PHASES TENANTS=$TENANTS DRAIN=${DRAIN:-4m} OUT="$OUT" COORDINATOR_URL=http://127.0.0.1:19000 \
  SANDBOX_URL=$SBX CI_URL=$CI METRICS="$metrics" driver > "$OUT/driver.log" 2>&1
echo "ended=$(date -u +%FT%TZ)" >> "$OUT/run.txt"
echo "done: $OUT"
