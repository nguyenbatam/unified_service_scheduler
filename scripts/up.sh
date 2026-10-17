#!/usr/bin/env bash
# Build and start the full stack; returns once every service with a healthcheck is healthy.
# Usage: ./scripts/up.sh [--keep]
#   default  start from an empty database: only the seed (demo dealerships, catalog, users, and a
#            few demo bookings placed from today). It deletes this project's Postgres volume, so every
#            booking, account, and stress/bench row made locally is lost. Metrics and dashboards are
#            kept.
#   --keep   keep the existing database; the seed only adds what is missing.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
fresh=true
case "${1:-}" in
  "" | --fresh) ;;
  --keep) fresh=false ;;
  *) echo "usage: $0 [--keep]" >&2; exit 2 ;;
esac
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"
if $fresh; then
  project="$(docker compose config --format json | python3 -c 'import json, sys; print(json.load(sys.stdin)["name"])')"
  echo "Starting fresh: removing the Postgres data of compose project '$project' (use --keep to keep it)"
  docker compose down
  # Found by label, so a custom project name (COMPOSE_PROJECT_NAME) still matches.
  volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.volume=pgdata")"
  if [ -n "$volumes" ]; then
    # shellcheck disable=SC2086 # one name per line, none with spaces
    docker volume rm $volumes >/dev/null
  fi
fi
docker compose up -d --build --wait --wait-timeout "${WAIT_TIMEOUT:-300}"
echo "API http://localhost:${GATEWAY_PORT}  UI http://localhost:${WEB_PORT}"
