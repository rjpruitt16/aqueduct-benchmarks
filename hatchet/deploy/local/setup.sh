#!/bin/sh
# Generates Hatchet's keys into hatchet.env, starts full self-hosted Hatchet
# (Postgres, migrations, seed, engine, API) configured only by environment
# variables, the same way it runs on Fly, and writes an API token to
# token.txt.
set -eu
umask 077 # hatchet.env and token.txt hold secrets
cd "$(dirname "$0")"
VERSION=v0.110.8
if [ ! -f hatchet.env ]; then
  keys=$(mktemp -d)
  docker run --rm -v "$keys:/keys" ghcr.io/hatchet-dev/hatchet/hatchet-admin:$VERSION \
    /hatchet/hatchet-admin keyset create-local-keys --key-dir /keys >/dev/null
  {
    echo "SERVER_ENCRYPTION_MASTER_KEYSET=$(cat "$keys/master.key")"
    echo "SERVER_ENCRYPTION_JWT_PRIVATE_KEYSET=$(cat "$keys/private_ec256.key")"
    echo "SERVER_ENCRYPTION_JWT_PUBLIC_KEYSET=$(cat "$keys/public_ec256.key")"
    echo "SERVER_AUTH_COOKIE_SECRETS=$(openssl rand -hex 8) $(openssl rand -hex 8)"
  } > hatchet.env
  chmod 600 hatchet.env
  rm -rf "$keys"
fi
docker compose up -d
tries=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/api/ready)" = 200 ]; do
  tries=$((tries + 1))
  if [ "$tries" -ge 90 ]; then
    echo "Hatchet API not ready after 3 minutes:" >&2
    docker compose ps >&2
    exit 1
  fi
  sleep 2
done
out=$(docker compose run --rm --no-deps seed /hatchet/hatchet-admin token create \
  --tenant-id 707d0855-80ab-4e1f-a156-f1c4546cbf52 2>/dev/null)
token=$(printf '%s\n' "$out" | tail -1)
case "$token" in
  *.*.*) ;;
  *) echo "token creation failed: $out" >&2; exit 1 ;;
esac
[ -f token.txt ] && chmod 600 token.txt
printf '%s\n' "$token" > token.txt
echo "Hatchet is up: REST http://localhost:8080, gRPC localhost:7077, token in $(pwd)/token.txt"
