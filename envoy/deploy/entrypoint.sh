#!/bin/sh
set -e
case "${ROLE:-}" in
  backend) exec backend ;;
  client) exec client ;;
  aquifer) exec aquifer ;;
  *) echo "ROLE must be backend, client or aquifer" >&2; exit 2 ;;
esac
