#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${HOMEAGENT_ENV_FILE:-$ROOT_DIR/tools/.env.local}"
ACTION="${1:-get}"

usage() {
  cat <<USAGE
Usage: tools/config_homeagent.sh [get|set|reset]

Configure StackChan HomeAgent over BLE.

Before running 'set':
  cp tools/.env.example tools/.env.local
  edit tools/.env.local

Environment:
  HOMEAGENT_ENV_FILE  Override env file path. Default: tools/.env.local
USAGE
}

case "$ACTION" in
  get|set|reset) ;;
  -h|--help|help) usage; exit 0 ;;
  *) usage; exit 1 ;;
esac

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
elif [[ "$ACTION" == "set" ]]; then
  echo "Missing env file: $ENV_FILE" >&2
  echo "Create it with: cp tools/.env.example tools/.env.local" >&2
  exit 1
fi

if [[ "$ACTION" == "set" ]]; then
  : "${RELAY_URL:?RELAY_URL is required in $ENV_FILE}"
  : "${RELAY_TOKEN:?RELAY_TOKEN is required in $ENV_FILE}"
  : "${DEVICE_ID:?DEVICE_ID is required in $ENV_FILE}"
fi

exec swift "$ROOT_DIR/tools/ble_config_homeagent.swift" "$ACTION"
