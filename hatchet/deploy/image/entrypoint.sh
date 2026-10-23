#!/bin/sh
# ROLE: worker | driver | aquifer | pgsampler.
# The driver role also runs the Postgres sampler alongside it when
# DATABASE_URL is set, and stops it when the driver finishes.
set -eu
case "${ROLE:-}" in
  worker) exec worker ;;
  aquifer) exec aquifer ;;
  pgsampler) exec pgsampler ;;
  driver)
    sampler=""
    if [ -n "${DATABASE_URL:-}" ]; then
      pgsampler & sampler=$!
    fi
    status=0
    driver > "${OUT:-out}/driver.log" 2>&1 || status=$?
    if [ -n "$sampler" ]; then
      kill -INT "$sampler" 2>/dev/null || true
      wait "$sampler" || true
    fi
    # HOLD=1 keeps the machine up after the run so the results can be copied
    # off; DONE marks that they are complete.
    if [ "${HOLD:-}" = 1 ]; then
      echo "$status" > "${OUT:-out}/DONE"
      exec sleep 2147483647
    fi
    exit "$status"
    ;;
  *) echo "set ROLE to worker, driver, aquifer or pgsampler" >&2; exit 2 ;;
esac
