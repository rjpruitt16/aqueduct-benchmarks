#!/bin/sh
set -e
case "${ROLE:-}" in
  backend) exec backend ;;
  client) exec client ;;
  service) exec service ;;
  dispatcher) exec dispatcher ;;
  aquifer) exec aquifer ;;
  *) echo "ROLE must be backend, client, service, dispatcher or aquifer" >&2; exit 2 ;;
esac
