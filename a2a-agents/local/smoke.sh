#!/bin/sh
# Local smoke test: one full A2A workflow set in configuration A, B or C, on
# one machine with Docker and Go, using the stand-in model. It checks the
# plumbing and the accounting, not performance.
#
# Every issue in the corpus runs once (COUNT=8 workflows, three of which need a
# second round), at the same time, with capacity set tight (2 model
# sequences, 2 sandbox slots, a pool of 4) so calls queue, get refused and are
# retried.
#
# Usage, from a2a-agents/: local/smoke.sh <A|B|C>
# Results: .smoke/<config>/out (summary.json, ledger.jsonl, metrics_end.txt).
set -eu
CONFIG=${1:?usage: smoke.sh <A|B|C>}
case "$CONFIG" in A) MODE=direct ;; B) MODE=queue ;; C) MODE=aquifer ;; *) echo "config must be A, B or C" >&2; exit 2 ;; esac
ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$ROOT/.smoke/$CONFIG
BIN=$ROOT/.smoke/bin
AQUIFER_REF=${AQUIFER_REF:-main}
rm -rf "${W:?}"
mkdir -p "$W/out" "$BIN"
printf 'config=%s\nmode=%s\nlocal=1\n' "$CONFIG" "$MODE" > "$W/out/run.txt"

(cd "$ROOT" && go build -o "$BIN/" ./cmd/...)
if [ "$CONFIG" = C ]; then
  AQSRC=$ROOT/.smoke/aquifer-src
  [ -d "$AQSRC/.git" ] || git clone -q https://github.com/rjpruitt16/aquifer "$AQSRC"
  (cd "$AQSRC" && git fetch -q origin && git checkout -q "origin/$AQUIFER_REF" 2>/dev/null || git checkout -q "$AQUIFER_REF"; go build -o "$BIN/aquifer" ./cmd/aquifer)
fi
docker pull -q python:3.12-alpine >/dev/null

pids=""
cleanup() {
  for p in $pids; do kill "$p" 2>/dev/null || true; done
  docker rm -f a2a-pg a2a-redis >/dev/null 2>&1 || true
}
trap cleanup EXIT
# start <name> <program> [VAR=value...]: run program with those settings.
start() { name=$1 prog=$2; shift 2; env "$@" "$prog" > "$W/$name.log" 2>&1 & pids="$pids $!"; }

docker rm -f a2a-pg a2a-redis >/dev/null 2>&1 || true
docker run -d --name a2a-pg -p 127.0.0.1:15432:5432 -e POSTGRES_PASSWORD=a2a -e POSTGRES_DB=a2a postgres:16.4 >/dev/null
i=0
until docker run --rm --network host -e PGPASSWORD=a2a postgres:16.4 pg_isready -h 127.0.0.1 -p 15432 -U postgres >/dev/null 2>&1; do
  i=$((i + 1)); [ "$i" -gt 60 ] && { echo "postgres didn't start" >&2; exit 1; }; sleep 2
done
if [ "$CONFIG" = B ]; then
  docker run -d --name a2a-redis -p 127.0.0.1:16379:6379 redis:7-alpine >/dev/null
fi

start mockllm "$BIN/mockllm" LISTEN=:18000 MAX_SEQS=2 TOKENS_PER_S=200
start dbtool "$BIN/dbtool" LISTEN=:18100 DATABASE_URL="postgres://postgres:a2a@127.0.0.1:15432/a2a?sslmode=disable" DB_POOL=4 DB_WAIT=500ms
start sandbox "$BIN/sandbox" LISTEN=:18200 SLOTS=2 RUNNER=docker LIMITS=${SANDBOX_LIMITS:-rlimit} USER=${SANDBOX_USER:-root} LOG="$W/out/executions.jsonl"
LLM=http://127.0.0.1:18000 DB=http://127.0.0.1:18100 SBX=http://127.0.0.1:18200
metrics="mockllm=$LLM/metrics,dbtool=$DB/metrics,sandbox=$SBX/metrics"
extra=""
case "$CONFIG" in
  B)
    start dispatcher "$BIN/dispatcher" LISTEN=:18300 REDIS_URL=redis://127.0.0.1:16379 RESOURCES=llm=2,db=4,sandbox=2
    extra="REDIS_URL=redis://127.0.0.1:16379"
    metrics="$metrics,dispatcher=http://127.0.0.1:18300/metrics" ;;
  C)
    # One upstream per resource, with the same limits B's dispatcher has.
    printf 'defaults:\n  rps: 2\n  max_concurrent: 1\nupstreams:\n  "127.0.0.1:18000":\n    rps: 100\n    max_concurrent: 2\n  "127.0.0.1:18100":\n    rps: 200\n    max_concurrent: 4\n  "127.0.0.1:18200":\n    rps: 100\n    max_concurrent: 2\n' > "$W/aquifer.yaml"
    start aquifer "$BIN/aquifer" AQUIFER_ADAPTER=http PORT=18400 DB_PATH="$W/aqdata" CONFIG_PATH="$W/aquifer.yaml"
    extra="AQUIFER_URL=http://127.0.0.1:18400"
    metrics="$metrics,aquifer=http://127.0.0.1:18400/metrics" ;;
esac
for a in research:19001 coding:19002 verify:19003; do
  start "${a%%:*}" "$BIN/agent" ROLE="${a%%:*}" LISTEN=":${a#*:}" PUBLIC_URL="http://127.0.0.1:${a#*:}" \
    RESOURCE_MODE=$MODE LLM_URL=$LLM DB_URL=$DB SANDBOX_URL=$SBX $extra
done
start coordinator "$BIN/agent" ROLE=coordinator LISTEN=:19000 PUBLIC_URL=http://127.0.0.1:19000 \
  RESEARCH_URL=http://127.0.0.1:19001 CODING_URL=http://127.0.0.1:19002 VERIFY_URL=http://127.0.0.1:19003

# Wait for every service to answer.
for u in $LLM/health $DB/health $SBX/health; do
  i=0; until curl -sf "$u" >/dev/null; do i=$((i + 1)); [ "$i" -gt 60 ] && { echo "$u didn't come up" >&2; exit 1; }; sleep 1; done
done
[ "$CONFIG" = C ] && { i=0; until curl -sf http://127.0.0.1:18400/health >/dev/null; do i=$((i + 1)); [ "$i" -gt 60 ] && exit 1; sleep 1; done; }
for p in 19000 19001 19002 19003; do
  i=0; until curl -sf "http://127.0.0.1:$p/.well-known/agent-card.json" >/dev/null; do i=$((i + 1)); [ "$i" -gt 30 ] && { echo "agent on $p didn't come up" >&2; exit 1; }; sleep 1; done
done

TENANTS=big:3,small-1:1,small-2:1 COUNT=${COUNT:-8} DRAIN=${DRAIN:-5m} OUT="$W/out" COORDINATOR_URL=http://127.0.0.1:19000 \
  SANDBOX_URL=$SBX METRICS="$metrics" "$BIN/driver" > "$W/out/driver.log" 2>&1
echo "done: $W/out"
